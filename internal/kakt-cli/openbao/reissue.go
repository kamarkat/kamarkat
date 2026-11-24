package openbao

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"sort"
	"strings"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
)

// ErrRolloverIncomplete: some certificate, consumer or trust bundle is not yet
// under the new root only. The rollover is finished by running the same command
// again.
var ErrRolloverIncomplete = errors.New("compromise rollover incomplete")

// LeafCert is one cert-manager Certificate issued by an OpenBao issuer.
type LeafCert struct {
	Namespace, Name, SecretName string
	// CA is its Secret's ca.crt: the trust anchor its holder verifies peers with.
	CA      []byte
	Ready   bool
	Issuing bool
	// NotBefore of the certificate currently issued (status.notBefore).
	NotBefore time.Time
}

func (l LeafCert) key() string { return l.Namespace + "/" + l.Name }

// Workload is what is restarted so a running process loads a reissued Secret.
type Workload struct{ Kind, Namespace, Name string }

func (w Workload) String() string { return w.Kind + "/" + w.Namespace + "/" + w.Name }

// Consumer is a workload with a running pod that uses Secret (in its namespace)
// by mount, environment or Reloader annotation, and that pod's start time.
type Consumer struct {
	Workload Workload
	Secret   string
	Started  time.Time
}

// LeafCluster is one cluster whose OpenBao-issued certificates are reissued.
type LeafCluster interface {
	Name() string
	Certificates(ctx context.Context) ([]LeafCert, error)
	Reissue(ctx context.Context, c LeafCert) error
	// Consumers lists the running consumers of the given Secrets (namespace/name).
	Consumers(ctx context.Context, secrets map[string]bool) ([]Consumer, error)
	Restart(ctx context.Context, w Workload) error
	// CompromisedCopies lists every Secret and ConfigMap holding root. Copies the
	// platform regenerates from a reissued Secret are removed first, once.
	CompromisedCopies(ctx context.Context, root []byte, retireTransit bool) ([]string, error)
}

// anchoredOnly reports whether ca holds exactly one certificate and it is anchor.
func anchoredOnly(ca, anchor []byte) bool {
	want, _ := pem.Decode(anchor)
	if want == nil {
		return false
	}
	var n int
	rest := ca
	for {
		var b *pem.Block
		b, rest = pem.Decode(rest)
		if b == nil {
			break
		}
		if b.Type != "CERTIFICATE" || !bytes.Equal(b.Bytes, want.Bytes) {
			return false
		}
		n++
	}
	return n == 1
}

// holdsCert reports whether text carries cert's DER in any PEM block, also when
// the PEM is indented inside a YAML document.
func holdsCert(text []byte, certDER []byte) bool {
	if !bytes.Contains(text, []byte("-----BEGIN CERTIFICATE-----")) {
		return false
	}
	lines := bytes.Split(text, []byte("\n"))
	for i := range lines {
		lines[i] = bytes.TrimLeft(lines[i], " \t")
	}
	rest := bytes.Join(lines, []byte("\n"))
	for {
		var b *pem.Block
		b, rest = pem.Decode(rest)
		if b == nil {
			return false
		}
		if bytes.Equal(b.Bytes, certDER) {
			return true
		}
	}
}

// freshAfter is how long after a certificate's notBefore a pod must have started
// to have loaded it: OpenBao backdates notBefore by 30s.
const freshAfter = 30 * time.Second

// ReissueLeaves completes a compromise rollover on every cluster, in order:
//  1. every OpenBao-issued certificate is reissued until its ca.crt is exactly
//     anchor;
//  2. every running consumer of a reissued Secret that started before it was
//     reissued is restarted, so no process keeps the compromised root in memory;
//  3. no Secret or ConfigMap anywhere still holds compromised.
//
// mTLS between a component that has the new root and one that has not is refused
// meanwhile, and no compatibility trust is added: that interruption is the stated
// cost of a compromise rollover.
func ReissueLeaves(ctx context.Context, clusters []LeafCluster, anchor, compromised []byte, wait, poll time.Duration, out io.Writer) error {
	if out == nil {
		out = io.Discard
	}
	oldRoot, _ := pem.Decode(compromised)
	if oldRoot == nil {
		return errors.New("the compromised root certificate is required to verify it is gone")
	}
	triggered := map[string]bool{}
	restarted := map[string]time.Time{}
	retired := map[string]bool{}
	deadline := time.Now().Add(wait)
	for {
		var pending []string
		for _, cl := range clusters {
			certs, err := cl.Certificates(ctx)
			if err != nil {
				return fmt.Errorf("cluster %s: %w", cl.Name(), err)
			}
			leavesDone := true
			issued := map[string]time.Time{}
			for _, c := range certs {
				if c.Ready && anchoredOnly(c.CA, anchor) {
					issued[c.Namespace+"/"+c.SecretName] = c.NotBefore
					continue
				}
				leavesDone = false
				id := cl.Name() + ":" + c.key()
				pending = append(pending, "certificate "+id)
				if triggered[id] || c.Issuing || anchoredOnly(c.CA, anchor) {
					continue
				}
				if err := cl.Reissue(ctx, c); err != nil {
					return fmt.Errorf("cluster %s: reissue %s: %w", cl.Name(), c.key(), err)
				}
				triggered[id] = true
				fmt.Fprintf(out, "  reissuing %s\n", id)
			}
			if !leavesDone {
				continue // consumers and bundles are judged once every leaf is new
			}

			secrets := map[string]bool{}
			for s := range issued {
				secrets[s] = true
			}
			consumers, err := cl.Consumers(ctx, secrets)
			if err != nil {
				return fmt.Errorf("cluster %s: consumers: %w", cl.Name(), err)
			}
			for _, c := range consumers {
				wid := cl.Name() + ":" + c.Workload.String()
				if c.Started.After(issued[c.Workload.Namespace+"/"+c.Secret].Add(freshAfter)) {
					continue
				}
				if at, ok := restarted[wid]; ok {
					if c.Started.Before(at) {
						pending = append(pending, "consumer "+wid+" (restarting)")
					}
					continue
				}
				pending = append(pending, "consumer "+wid)
				if err := cl.Restart(ctx, c.Workload); err != nil {
					return fmt.Errorf("cluster %s: restart %s: %w", cl.Name(), c.Workload, err)
				}
				restarted[wid] = time.Now()
				fmt.Fprintf(out, "  restarting %s (uses %s)\n", wid, c.Secret)
			}

			copies, err := cl.CompromisedCopies(ctx, compromised, !retired[cl.Name()])
			if err != nil {
				return fmt.Errorf("cluster %s: trust bundles: %w", cl.Name(), err)
			}
			retired[cl.Name()] = true
			for _, cp := range copies {
				pending = append(pending, "compromised root in "+cl.Name()+":"+cp)
			}
		}
		if len(pending) == 0 {
			fmt.Fprintf(out, "On %d cluster(s): every OpenBao-issued certificate and its running consumers are under the "+
				"new root only, and no Secret or ConfigMap holds the compromised root.\n", len(clusters))
			return nil
		}
		if time.Now().After(deadline) {
			sort.Strings(pending)
			return fmt.Errorf("%w: %d item(s) not yet under the new root only: %s. Re-run the same command "+
				"to continue", ErrRolloverIncomplete, len(pending), strings.Join(pending, "; "))
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(poll):
		}
	}
}

// KubectlLeafCluster reads and reissues certificates with kubectl, as
// cert-manager's own `cmctl renew` does: it sets the Issuing condition.
type KubectlLeafCluster struct {
	ClusterName, Kubeconfig string
}

func (k KubectlLeafCluster) Name() string { return k.ClusterName }

func (k KubectlLeafCluster) kubectl(ctx context.Context, args ...string) ([]byte, error) {
	if k.Kubeconfig != "" {
		args = append([]string{"--kubeconfig", k.Kubeconfig}, args...)
	}
	b, err := exec.CommandContext(ctx, "kubectl", args...).Output()
	if err != nil {
		var ee *exec.ExitError
		if errors.As(err, &ee) {
			return nil, fmt.Errorf("kubectl %s: %s", strings.Join(args, " "), strings.TrimSpace(string(ee.Stderr)))
		}
		return nil, err
	}
	return b, nil
}

func (k KubectlLeafCluster) getJSON(ctx context.Context, out any, args ...string) error {
	b, err := k.kubectl(ctx, append(args, "-o", "json")...)
	if err != nil {
		return err
	}
	return json.Unmarshal(b, out)
}

type cmCondition struct {
	Type   string `json:"type"`
	Status string `json:"status"`
}

func (k KubectlLeafCluster) Certificates(ctx context.Context) ([]LeafCert, error) {
	// The issuers backed by OpenBao (cert-manager's Vault issuer).
	vault := map[string]bool{}
	for _, kind := range []string{"clusterissuers.cert-manager.io", "issuers.cert-manager.io"} {
		var l struct {
			Items []struct {
				Kind     string `json:"kind"`
				Metadata struct {
					Name, Namespace string
				} `json:"metadata"`
				Spec struct {
					Vault json.RawMessage `json:"vault"`
				} `json:"spec"`
			} `json:"items"`
		}
		if err := k.getJSON(ctx, &l, "get", kind, "-A"); err != nil {
			return nil, err
		}
		for _, i := range l.Items {
			if len(i.Spec.Vault) > 0 {
				vault[i.Kind+"/"+i.Metadata.Namespace+"/"+i.Metadata.Name] = true
			}
		}
	}
	var certs struct {
		Items []struct {
			Metadata struct {
				Name, Namespace string
			} `json:"metadata"`
			Spec struct {
				SecretName string `json:"secretName"`
				IssuerRef  struct {
					Name, Kind string
				} `json:"issuerRef"`
			} `json:"spec"`
			Status struct {
				Conditions []cmCondition `json:"conditions"`
				NotBefore  time.Time     `json:"notBefore"`
			} `json:"status"`
		} `json:"items"`
	}
	if err := k.getJSON(ctx, &certs, "get", "certificates.cert-manager.io", "-A"); err != nil {
		return nil, err
	}
	var out []LeafCert
	for _, c := range certs.Items {
		kind, ns := c.Spec.IssuerRef.Kind, c.Metadata.Namespace
		if kind == "" {
			kind = "Issuer"
		}
		if kind == "ClusterIssuer" {
			ns = ""
		}
		if !vault[kind+"/"+ns+"/"+c.Spec.IssuerRef.Name] {
			continue
		}
		lc := LeafCert{Namespace: c.Metadata.Namespace, Name: c.Metadata.Name, SecretName: c.Spec.SecretName,
			NotBefore: c.Status.NotBefore}
		for _, cond := range c.Status.Conditions {
			switch cond.Type {
			case "Ready":
				lc.Ready = cond.Status == "True"
			case "Issuing":
				lc.Issuing = cond.Status == "True"
			}
		}
		raw, err := k.kubectl(ctx, "get", "secret", c.Spec.SecretName, "-n", c.Metadata.Namespace,
			"--ignore-not-found", "-o", `jsonpath={.data.ca\.crt}`)
		if err != nil {
			return nil, err
		}
		if s := strings.TrimSpace(string(raw)); s != "" {
			if lc.CA, err = base64.StdEncoding.DecodeString(s); err != nil {
				return nil, fmt.Errorf("secret %s/%s ca.crt: %w", c.Metadata.Namespace, c.Spec.SecretName, err)
			}
		}
		out = append(out, lc)
	}
	return out, nil
}

func (k KubectlLeafCluster) Reissue(ctx context.Context, c LeafCert) error {
	cond, _ := json.Marshal(map[string]any{
		"type": "Issuing", "status": "True", "reason": "ManuallyTriggered",
		"message":            "Reissued by a compromise rollover (kakt pki sign-intermediate --compromised)",
		"lastTransitionTime": time.Now().UTC().Format(time.RFC3339),
	})
	patch := fmt.Sprintf(`[{"op":"add","path":"/status/conditions/-","value":%s}]`, cond)
	_, err := k.kubectl(ctx, "patch", "certificates.cert-manager.io", c.Name, "-n", c.Namespace,
		"--subresource=status", "--type=json", "-p", patch)
	if err != nil && strings.Contains(err.Error(), "/status/conditions") {
		// Never issued yet: no conditions to append to; cert-manager is issuing it
		// under the new default issuer anyway.
		return nil
	}
	return err
}

type ownerRef struct {
	Kind       string `json:"kind"`
	Name       string `json:"name"`
	Controller bool   `json:"controller"`
}

type podList struct {
	Items []struct {
		Metadata struct {
			Name, Namespace string
			OwnerReferences []ownerRef `json:"ownerReferences"`
		} `json:"metadata"`
		Spec struct {
			Volumes []struct {
				Secret *struct {
					SecretName string `json:"secretName"`
				} `json:"secret"`
				Projected *struct {
					Sources []struct {
						Secret *struct {
							Name string `json:"name"`
						} `json:"secret"`
					} `json:"sources"`
				} `json:"projected"`
			} `json:"volumes"`
			Containers     []podContainer `json:"containers"`
			InitContainers []podContainer `json:"initContainers"`
		} `json:"spec"`
		Status struct {
			Phase     string     `json:"phase"`
			StartTime *time.Time `json:"startTime"`
		} `json:"status"`
	} `json:"items"`
}

type podContainer struct {
	Env []struct {
		ValueFrom *struct {
			SecretKeyRef *struct {
				Name string `json:"name"`
			} `json:"secretKeyRef"`
		} `json:"valueFrom"`
	} `json:"env"`
	EnvFrom []struct {
		SecretRef *struct {
			Name string `json:"name"`
		} `json:"secretRef"`
	} `json:"envFrom"`
}

type workloadList struct {
	Items []struct {
		Kind     string `json:"kind"`
		Metadata struct {
			Name, Namespace string
			Annotations     map[string]string `json:"annotations"`
			OwnerReferences []ownerRef        `json:"ownerReferences"`
		} `json:"metadata"`
	} `json:"items"`
}

func controllerOf(refs []ownerRef) *ownerRef {
	for i := range refs {
		if refs[i].Controller {
			return &refs[i]
		}
	}
	return nil
}

func (k KubectlLeafCluster) Consumers(ctx context.Context, secrets map[string]bool) ([]Consumer, error) {
	var pods podList
	if err := k.getJSON(ctx, &pods, "get", "pods", "-A"); err != nil {
		return nil, err
	}
	var rs, wl workloadList
	if err := k.getJSON(ctx, &rs, "get", "replicasets", "-A"); err != nil {
		return nil, err
	}
	if err := k.getJSON(ctx, &wl, "get", "deployments,statefulsets,daemonsets", "-A"); err != nil {
		return nil, err
	}
	rsOwner := map[string]*ownerRef{}
	for _, r := range rs.Items {
		rsOwner[r.Metadata.Namespace+"/"+r.Metadata.Name] = controllerOf(r.Metadata.OwnerReferences)
	}
	// Secrets a workload is reloaded for by annotation: it reads them through the
	// API rather than a mount (the Argo CD principal does).
	annotated := map[string][]string{}
	for _, w := range wl.Items {
		id := w.Kind + "/" + w.Metadata.Namespace + "/" + w.Metadata.Name
		for _, s := range strings.Split(w.Metadata.Annotations["secret.reloader.stakater.com/reload"], ",") {
			if s = strings.TrimSpace(s); s != "" {
				annotated[id] = append(annotated[id], s)
			}
		}
	}
	var out []Consumer
	for _, p := range pods.Items {
		if p.Status.Phase != "Running" || p.Status.StartTime == nil {
			continue
		}
		ns := p.Metadata.Namespace
		w := Workload{Kind: "Pod", Namespace: ns, Name: p.Metadata.Name}
		if o := controllerOf(p.Metadata.OwnerReferences); o != nil {
			w = Workload{Kind: o.Kind, Namespace: ns, Name: o.Name}
			if o.Kind == "ReplicaSet" {
				if d := rsOwner[ns+"/"+o.Name]; d != nil {
					w = Workload{Kind: d.Kind, Namespace: ns, Name: d.Name}
				}
			}
			if o.Kind == "Job" {
				continue // runs to completion; a new run reads the new Secret
			}
		}
		used := map[string]bool{}
		for _, v := range p.Spec.Volumes {
			if v.Secret != nil {
				used[v.Secret.SecretName] = true
			}
			if v.Projected != nil {
				for _, s := range v.Projected.Sources {
					if s.Secret != nil {
						used[s.Secret.Name] = true
					}
				}
			}
		}
		for _, c := range append(append([]podContainer{}, p.Spec.Containers...), p.Spec.InitContainers...) {
			for _, e := range c.Env {
				if e.ValueFrom != nil && e.ValueFrom.SecretKeyRef != nil {
					used[e.ValueFrom.SecretKeyRef.Name] = true
				}
			}
			for _, e := range c.EnvFrom {
				if e.SecretRef != nil {
					used[e.SecretRef.Name] = true
				}
			}
		}
		for _, s := range annotated[w.String()] {
			used[s] = true
		}
		for s := range used {
			if secrets[ns+"/"+s] {
				out = append(out, Consumer{Workload: w, Secret: s, Started: *p.Status.StartTime})
			}
		}
	}
	return out, nil
}

func (k KubectlLeafCluster) Restart(ctx context.Context, w Workload) error {
	switch w.Kind {
	case "Deployment", "StatefulSet", "DaemonSet":
		_, err := k.kubectl(ctx, "rollout", "restart", strings.ToLower(w.Kind)+"/"+w.Name, "-n", w.Namespace)
		return err
	case "Cluster": // CloudNativePG: its own rolling restart (`kubectl cnpg restart`)
		_, err := k.kubectl(ctx, "annotate", "clusters.postgresql.cnpg.io", w.Name, "-n", w.Namespace,
			"--overwrite", "kubectl.kubernetes.io/restartedAt="+time.Now().UTC().Format(time.RFC3339))
		return err
	default:
		return fmt.Errorf("%s is not a workload this command restarts; restart it yourself and re-run", w)
	}
}

// crsWrapperType is a ClusterResourceSet wrapper: hub-operator regenerates the
// spoke bootstrap wrappers from their (reissued) TLS Secret when one is absent.
const crsWrapperType = "addons.cluster.x-k8s.io/resource-set"

func (k KubectlLeafCluster) CompromisedCopies(ctx context.Context, root []byte, retireTransit bool) ([]string, error) {
	blk, _ := pem.Decode(root)
	if blk == nil {
		return nil, errors.New("no compromised root certificate given")
	}
	var found []string
	for _, kind := range []string{"secrets", "configmaps"} {
		var l struct {
			Items []struct {
				Metadata struct {
					Name, Namespace string
				} `json:"metadata"`
				Type       string            `json:"type"`
				Data       map[string]string `json:"data"`
				BinaryData map[string]string `json:"binaryData"`
			} `json:"items"`
		}
		if err := k.getJSON(ctx, &l, "get", kind, "-A"); err != nil {
			return nil, err
		}
		for _, o := range l.Items {
			hit := false
			for _, v := range o.Data {
				text := []byte(v)
				if kind == "secrets" {
					if text, _ = base64.StdEncoding.DecodeString(v); text == nil {
						continue
					}
				}
				if holdsCert(text, blk.Bytes) {
					hit = true
					break
				}
			}
			if !hit {
				continue
			}
			ref := strings.TrimSuffix(kind, "s") + " " + o.Metadata.Namespace + "/" + o.Metadata.Name
			if retireTransit && kind == "secrets" && o.Type == crsWrapperType && o.Metadata.Namespace == "platform-capi" {
				if _, err := k.kubectl(ctx, "delete", "secret", o.Metadata.Name, "-n", o.Metadata.Namespace,
					"--ignore-not-found"); err != nil {
					return nil, err
				}
				ref += " (removed; hub-operator regenerates it)"
			}
			found = append(found, ref)
		}
	}
	return found, nil
}

// FleetLeafClusters is the hub plus every SpokePool's cluster, reached through
// the CAPI kubeconfig the hub holds for it. cleanup removes the temporary
// kubeconfig files.
func FleetLeafClusters(ctx context.Context, cs kubernetes.Interface, hubKubeconfig string) ([]LeafCluster, func(), error) {
	hub := KubectlLeafCluster{ClusterName: "hub", Kubeconfig: hubKubeconfig}
	clusters := []LeafCluster{hub}
	var files []string
	cleanup := func() {
		for _, f := range files {
			_ = os.Remove(f)
		}
	}
	var pools struct {
		Items []struct {
			Metadata struct{ Name string } `json:"metadata"`
		} `json:"items"`
	}
	if err := hub.getJSON(ctx, &pools, "get", "spokepools.nutgraf.in"); err != nil {
		return nil, cleanup, err
	}
	for _, p := range pools.Items {
		name := p.Metadata.Name
		sec, err := cs.CoreV1().Secrets("platform-capi").Get(ctx, name+"-kubeconfig", metav1.GetOptions{})
		if err != nil {
			cleanup()
			return nil, func() {}, fmt.Errorf("spoke %s: read platform-capi/%s-kubeconfig: %w", name, name, err)
		}
		f, err := os.CreateTemp("", "kakt-spoke-*.kubeconfig")
		if err != nil {
			cleanup()
			return nil, func() {}, err
		}
		files = append(files, f.Name())
		_, werr := f.Write(sec.Data["value"])
		if cerr := f.Close(); werr == nil {
			werr = cerr
		}
		if werr != nil {
			cleanup()
			return nil, func() {}, werr
		}
		clusters = append(clusters, KubectlLeafCluster{ClusterName: name, Kubeconfig: f.Name()})
	}
	return clusters, cleanup, nil
}
