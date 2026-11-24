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

// ErrRolloverIncomplete: some leaves do not yet carry only the new root. The
// rollover is finished by running the same command again.
var ErrRolloverIncomplete = errors.New("compromise rollover incomplete")

// LeafCert is one cert-manager Certificate issued by an OpenBao issuer.
type LeafCert struct {
	Namespace, Name string
	// CA is its Secret's ca.crt: the trust anchor its holder verifies peers with.
	CA      []byte
	Ready   bool
	Issuing bool
}

func (l LeafCert) key() string { return l.Namespace + "/" + l.Name }

// LeafCluster is one cluster whose OpenBao-issued certificates are reissued.
type LeafCluster interface {
	Name() string
	Certificates(ctx context.Context) ([]LeafCert, error)
	Reissue(ctx context.Context, c LeafCert) error
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

// ReissueLeaves reissues every OpenBao-issued certificate whose ca.crt is not
// exactly anchor, on every cluster, and waits (bounded) for each to be Ready under
// it. mTLS between a component that has the new root and one that has not is
// refused meanwhile: that interruption is the stated cost of a compromise
// rollover.
func ReissueLeaves(ctx context.Context, clusters []LeafCluster, anchor []byte, wait, poll time.Duration, out io.Writer) error {
	if out == nil {
		out = io.Discard
	}
	triggered := map[string]bool{}
	deadline := time.Now().Add(wait)
	for {
		var pending []string
		for _, cl := range clusters {
			certs, err := cl.Certificates(ctx)
			if err != nil {
				return fmt.Errorf("cluster %s: %w", cl.Name(), err)
			}
			for _, c := range certs {
				if c.Ready && anchoredOnly(c.CA, anchor) {
					continue
				}
				id := cl.Name() + ":" + c.key()
				pending = append(pending, id)
				if triggered[id] || c.Issuing || anchoredOnly(c.CA, anchor) {
					continue
				}
				if err := cl.Reissue(ctx, c); err != nil {
					return fmt.Errorf("cluster %s: reissue %s: %w", cl.Name(), c.key(), err)
				}
				triggered[id] = true
				fmt.Fprintf(out, "  reissuing %s\n", id)
			}
		}
		if len(pending) == 0 {
			fmt.Fprintf(out, "Every OpenBao-issued certificate on %d cluster(s) now carries only the new root.\n", len(clusters))
			return nil
		}
		if time.Now().After(deadline) {
			sort.Strings(pending)
			return fmt.Errorf("%w: %d certificate(s) not yet reissued under the new root: %s. Re-run the "+
				"same command to continue", ErrRolloverIncomplete, len(pending), strings.Join(pending, ", "))
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

type cmCondition struct {
	Type   string `json:"type"`
	Status string `json:"status"`
}

func (k KubectlLeafCluster) Certificates(ctx context.Context) ([]LeafCert, error) {
	// The issuers backed by OpenBao (cert-manager's Vault issuer).
	vault := map[string]bool{}
	for _, kind := range []string{"clusterissuers.cert-manager.io", "issuers.cert-manager.io"} {
		b, err := k.kubectl(ctx, "get", kind, "-A", "-o", "json")
		if err != nil {
			return nil, err
		}
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
		if err := json.Unmarshal(b, &l); err != nil {
			return nil, err
		}
		for _, i := range l.Items {
			if len(i.Spec.Vault) > 0 {
				vault[i.Kind+"/"+i.Metadata.Namespace+"/"+i.Metadata.Name] = true
			}
		}
	}
	b, err := k.kubectl(ctx, "get", "certificates.cert-manager.io", "-A", "-o", "json")
	if err != nil {
		return nil, err
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
			} `json:"status"`
		} `json:"items"`
	}
	if err := json.Unmarshal(b, &certs); err != nil {
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
		lc := LeafCert{Namespace: c.Metadata.Namespace, Name: c.Metadata.Name}
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
	b, err := hub.kubectl(ctx, "get", "spokepools.nutgraf.in", "-o", "json")
	if err != nil {
		return nil, cleanup, err
	}
	var pools struct {
		Items []struct {
			Metadata struct{ Name string } `json:"metadata"`
		} `json:"items"`
	}
	if err := json.Unmarshal(b, &pools); err != nil {
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
