package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"sort"
	"strconv"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/kamarkat/kamarkat/internal/kakt-cli/hubaccess"
	"github.com/spf13/cobra"
)

// Cluster API Provider Hetzner's own labels, read from v1.0.7's api/v1beta1/tags.go.
//
// These are the identifying relationships the DEPLOYED provider actually uses: its
// `findServer` resolves a provider ID first and, failing that, lists servers by this
// exact label set. So matching on them is matching on what the controller will do,
// which is the point -- a server that merely has a similar name is not a match, and
// would be duplicated the moment reconciliation resumed.
const (
	caphOwnerLabelPrefix = "caph-cluster-" // + HetznerCluster name, value "owned"
	caphOwnedValue       = "owned"
	caphMachineNameLabel = "machine.caph-name"
	caphMachineTypeLabel = "machine_type"
	hcloudAPI            = "https://api.hetzner.cloud/v1"
)

type matchOpts struct {
	cluster, kubeconfig, kubeContext, token string
	// gitops and box say where the box declares its escrow, which is where the
	// provider credential comes from.
	gitops, box string
}

// newClusterMatchInventoryCmd compares recovered CAPI objects with the servers that
// actually exist, before anything is allowed to reconcile them.
//
// ADR-111 §5: the provider's inventory is the one authority a lost hub cannot take
// with it, so it -- not the backup -- is what says which machine is which server.
// This is the step between restoring a paused object graph and releasing it.
//
// It classifies and refuses; it never adopts, creates or deletes. Every unresolved
// row is for a person to resolve, because every automatic resolution is an invented
// adoption protocol and the provider offers no supported one (ADR-111 §3).
func newClusterMatchInventoryCmd() *cobra.Command {
	var o matchOpts

	cmd := &cobra.Command{
		Use:   "match-inventory",
		Short: "compare a cluster's recovered CAPI machines with the provider's servers",
		Long: `Lists the HCloudMachines this hub holds for a cluster and the servers the provider
actually has, and classifies every relationship between them.

Matched means one server carries this machine's name in the provider's own labels and,
where the object already names a provider ID, that ID is the same server.

Anything else is unresolved: an object with no server, a server with no object, or an
ambiguous pair. Unresolved rows stay unresolved -- nothing here adopts, creates or
remediates, because the deployed provider defines no supported adoption contract.

Read-only. It changes nothing in either the cluster or the provider.`,
		RunE: func(c *cobra.Command, _ []string) error {
			return runClusterMatchInventory(c.Context(), o)
		},
	}

	f := cmd.Flags()
	f.StringVar(&o.cluster, "cluster", "", "the cluster whose machines to match (required)")
	f.StringVar(&o.kubeconfig, "kubeconfig", "", "kubeconfig for the hub holding platform-capi")
	f.StringVar(&o.kubeContext, "context", "", "kubeconfig context")
	f.StringVar(&o.gitops, "gitops-dir", ".",
		"the box's repository, which declares its escrow and its box id")
	f.StringVar(&o.box, "box-cluster", "",
		"the management cluster whose declaration names this box's escrow (default: --cluster)")
	_ = cmd.MarkFlagRequired("cluster")
	return cmd
}

type hcloudServer struct {
	ID     int64             `json:"id"`
	Name   string            `json:"name"`
	Status string            `json:"status"`
	Labels map[string]string `json:"labels"`
}

type hcloudMachine struct {
	Name       string
	ProviderID string
}

func runClusterMatchInventory(ctx context.Context, o matchOpts) error {
	if ctx == nil {
		ctx = context.Background()
	}
	// The box's own credential, from its escrow. The provider's inventory is the
	// authority this compares against, so it has to be THIS box's provider
	// project: a token for another one answers truthfully about the wrong
	// account, and every machine here would read as missing.
	//
	// Not read from the cluster either, which is the point of the command --
	// during a reconstruction the cluster's copy is itself restored state.
	declaring := strings.TrimSpace(o.box)
	if declaring == "" {
		declaring = o.cluster
	}
	token, err := hubaccess.ProviderToken(ctx, o.gitops, declaring, "hetzner")
	if err != nil {
		return err
	}
	o.token = token

	hetznerCluster, err := o.hetznerClusterName(ctx)
	if err != nil {
		return err
	}
	machines, err := o.hcloudMachines(ctx)
	if err != nil {
		return err
	}
	servers, err := o.servers(ctx, hetznerCluster)
	if err != nil {
		return err
	}

	rows, matched, unresolved := classifyInventory(machines, servers)

	w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintf(w, "cluster %s, HetznerCluster %s\n\n", o.cluster, hetznerCluster)
	fmt.Fprintln(w, "OBJECT\tSERVER\tSTATE\tDETAIL")
	for _, r := range rows {
		fmt.Fprintf(w, "%s\t%s\t%s\t%s\n", r.Object, r.Server, r.State, r.Detail)
	}
	_ = w.Flush()

	fmt.Printf("\n%d matched, %d unresolved, from %d object(s) and %d server(s).\n",
		matched, unresolved, len(machines), len(servers))

	// An EMPTY comparison is not a match, and reporting it as one is the false green
	// this whole command exists to prevent. Nothing recovered and nothing at the
	// provider satisfies "no unresolved rows" trivially, so it has to be refused
	// separately -- the lesson the reference operator encodes as isSkipAllRestores:
	// a run that did nothing must not read as a run that succeeded.
	if len(machines) == 0 {
		return fmt.Errorf("no HCloudMachine on this hub carries cluster-name=%s, so there is "+
			"nothing to match and this is NOT a matched fleet. Either the management state has "+
			"not been restored yet, or it was restored under a different cluster name; in both "+
			"cases Cluster API must stay paused", o.cluster)
	}

	if unresolved > 0 {
		return fmt.Errorf("%d relationship(s) are unresolved, so Cluster API must stay paused "+
			"for %s. Releasing it now would provision a parallel fleet for the objects with no "+
			"server and remediate the servers with no object", unresolved, o.cluster)
	}
	fmt.Printf("\nEvery machine is matched. With the trust roots in place, Cluster API may be "+
		"released for %s -- leaves first, creators last (ADR-111 §3).\n", o.cluster)
	return nil
}

// inventoryRow is one relationship between what this hub holds and what the
// provider has.
type inventoryRow struct{ Object, Server, State, Detail string }

// Inventory states. Only matched is eligible for release; every other state is
// unresolved and stays that way, because the deployed provider defines no supported
// adoption contract and every automatic resolution would be an invented one.
const (
	stateMatched             = "matched"
	stateObjectWithoutServer = "object-without-server"
	stateServerWithoutObject = "server-without-object"
	stateAmbiguous           = "ambiguous"
	stateConflicting         = "conflicting"
	stateUnattributable      = "unattributable"
)

// classifyInventory decides every relationship from the two inventories alone.
//
// Separated from the fetching so the classification can be asserted without a
// cluster or a provider account: these are the decisions that keep a reconstruction
// from provisioning a parallel fleet, and they are the part worth testing.
func classifyInventory(machines []hcloudMachine, servers []hcloudServer) (rows []inventoryRow, matched, unresolved int) {
	// Index by the name CAPH stamped on the server. More than one under a name is
	// ambiguous by itself: the controller's own lookup raises MultipleInstances for
	// exactly this and refuses.
	byMachineName := map[string][]hcloudServer{}
	for _, s := range servers {
		if n := s.Labels[caphMachineNameLabel]; n != "" {
			byMachineName[n] = append(byMachineName[n], s)
		}
	}

	claimed := map[string]bool{}
	for _, m := range machines {
		claimed[m.Name] = true
		found := byMachineName[m.Name]

		switch {
		case len(found) == 0:
			unresolved++
			rows = append(rows, inventoryRow{m.Name, "-", stateObjectWithoutServer,
				"the server was deleted after the management-state backup, or this object " +
					"belongs to another cluster"})
		case len(found) > 1:
			unresolved++
			ids := make([]string, 0, len(found))
			for _, s := range found {
				ids = append(ids, strconv.FormatInt(s.ID, 10))
			}
			rows = append(rows, inventoryRow{m.Name, strings.Join(ids, ","), stateAmbiguous,
				"more than one server carries this machine name; the provider's own lookup " +
					"refuses this too"})
		default:
			s := found[0]
			want, hasID := serverIDFromProviderID(m.ProviderID)
			if hasID && want != s.ID {
				unresolved++
				rows = append(rows, inventoryRow{m.Name, strconv.FormatInt(s.ID, 10), stateConflicting,
					fmt.Sprintf("the object names server %d but the labels point at %d", want, s.ID)})
				continue
			}
			matched++
			detail := "labels and provider id agree"
			if !hasID {
				detail = "labels agree; the object names no provider id yet"
			}
			if t := s.Labels[caphMachineTypeLabel]; t != "" {
				detail += ", machine_type=" + t
			}
			rows = append(rows, inventoryRow{m.Name, strconv.FormatInt(s.ID, 10), stateMatched, detail})
		}
	}

	for _, s := range servers {
		n := s.Labels[caphMachineNameLabel]
		if n == "" {
			unresolved++
			rows = append(rows, inventoryRow{"-", strconv.FormatInt(s.ID, 10), stateUnattributable,
				"carries this cluster's owner label but no machine name, so no object can " +
					"be correlated with it"})
			continue
		}
		if !claimed[n] {
			unresolved++
			rows = append(rows, inventoryRow{"-", strconv.FormatInt(s.ID, 10), stateServerWithoutObject,
				fmt.Sprintf("a machine named %s exists at the provider and not on this hub: "+
					"created after the backup, and not adoptable because its bootstrap data "+
					"was never captured", n)})
		}
	}

	sort.Slice(rows, func(i, j int) bool {
		if rows[i].State != rows[j].State {
			return rows[i].State < rows[j].State
		}
		return rows[i].Object < rows[j].Object
	})
	return rows, matched, unresolved
}

// serverIDFromProviderID reads the provider id CAPH writes, `hcloud://<id>`.
func serverIDFromProviderID(providerID string) (int64, bool) {
	const prefix = "hcloud://"
	if !strings.HasPrefix(providerID, prefix) {
		return 0, false
	}
	id, err := strconv.ParseInt(strings.TrimPrefix(providerID, prefix), 10, 64)
	if err != nil {
		return 0, false
	}
	return id, true
}

func (o matchOpts) hetznerClusterName(ctx context.Context) (string, error) {
	raw, err := kubectlList(ctx, o.kubeconfig, o.kubeContext, "hetznerclusters", o.cluster)
	if err != nil {
		return "", err
	}
	var list struct {
		Items []struct {
			Metadata struct{ Name string } `json:"metadata"`
		} `json:"items"`
	}
	if err := json.Unmarshal(raw, &list); err != nil {
		return "", fmt.Errorf("reading the HetznerCluster list: %w", err)
	}
	switch len(list.Items) {
	case 0:
		return "", fmt.Errorf("no HetznerCluster carries cluster-name=%s, so the label the "+
			"provider stamps on servers cannot be derived. Restore the infrastructure object "+
			"before matching", o.cluster)
	case 1:
		return list.Items[0].Metadata.Name, nil
	default:
		return "", fmt.Errorf("%d HetznerClusters carry cluster-name=%s; which one owns the "+
			"servers is ambiguous and must be resolved first", len(list.Items), o.cluster)
	}
}

func (o matchOpts) hcloudMachines(ctx context.Context) ([]hcloudMachine, error) {
	raw, err := kubectlList(ctx, o.kubeconfig, o.kubeContext, "hcloudmachines", o.cluster)
	if err != nil {
		return nil, err
	}
	var list struct {
		Items []struct {
			Metadata struct{ Name string } `json:"metadata"`
			Spec     struct {
				ProviderID string `json:"providerID"`
			} `json:"spec"`
		} `json:"items"`
	}
	if err := json.Unmarshal(raw, &list); err != nil {
		return nil, fmt.Errorf("reading the HCloudMachine list: %w", err)
	}
	out := make([]hcloudMachine, 0, len(list.Items))
	for _, it := range list.Items {
		out = append(out, hcloudMachine{Name: it.Metadata.Name, ProviderID: it.Spec.ProviderID})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

// servers lists what the provider actually has for this cluster, by the owner label
// CAPH stamps. Paginated, because a fleet larger than one page would otherwise be
// reported as servers-without-objects that do not exist.
func (o matchOpts) servers(ctx context.Context, hetznerCluster string) ([]hcloudServer, error) {
	selector := caphOwnerLabelPrefix + hetznerCluster + "=" + caphOwnedValue
	client := &http.Client{Timeout: 30 * time.Second}

	var all []hcloudServer
	for page := 1; ; page++ {
		q := url.Values{}
		q.Set("label_selector", selector)
		q.Set("page", strconv.Itoa(page))
		q.Set("per_page", "50")

		req, err := http.NewRequestWithContext(ctx, http.MethodGet,
			hcloudAPI+"/servers?"+q.Encode(), nil)
		if err != nil {
			return nil, fmt.Errorf("build the provider request: %w", err)
		}
		req.Header.Set("Authorization", "Bearer "+o.token)

		resp, err := client.Do(req)
		if err != nil {
			return nil, fmt.Errorf("list the provider's servers: %w", err)
		}
		var body struct {
			Servers []hcloudServer `json:"servers"`
			Meta    struct {
				Pagination struct {
					NextPage *int `json:"next_page"`
				} `json:"pagination"`
			} `json:"meta"`
		}
		err = json.NewDecoder(resp.Body).Decode(&body)
		status := resp.StatusCode
		resp.Body.Close()

		if status == http.StatusUnauthorized || status == http.StatusForbidden {
			return nil, fmt.Errorf("the provider refused the token (%d); matching cannot "+
				"proceed on a partial inventory", status)
		}
		if status != http.StatusOK {
			return nil, fmt.Errorf("the provider refused the server list (%d)", status)
		}
		if err != nil {
			return nil, fmt.Errorf("reading the provider's server list: %w", err)
		}

		all = append(all, body.Servers...)
		if body.Meta.Pagination.NextPage == nil {
			break
		}
	}
	return all, nil
}

// kubectlList reads every object of a kind belonging to one cluster, by the label
// Cluster API itself sets. Read-only, and it returns an empty list rather than an
// error when there are none: nothing recovered yet is a state this command reports.
func kubectlList(ctx context.Context, kubeconfig, kubeContext, kind, cluster string) ([]byte, error) {
	args := []string{"get", kind, "-n", capiNamespace,
		"-l", capiClusterNameLabel + "=" + cluster, "-o", "json"}
	if kubeContext != "" {
		args = append([]string{"--context", kubeContext}, args...)
	}
	if kubeconfig != "" {
		args = append([]string{"--kubeconfig", kubeconfig}, args...)
	}
	out, err := exec.CommandContext(ctx, "kubectl", args...).Output()
	if err != nil {
		var ee *exec.ExitError
		if errors.As(err, &ee) && strings.Contains(string(ee.Stderr), "NotFound") {
			return []byte(`{"items":[]}`), nil
		}
		return nil, fmt.Errorf("kubectl get %s: %w", kind, err)
	}
	return out, nil
}
