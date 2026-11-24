// Package boxmcp is the box's platform state, served over the Model Context
// Protocol (ADR-116).
//
// It READS. Every tool lists, gets or watches what the box's controllers
// already own -- Applications, claims, clusters, certificates, backups -- and
// returns it shaped for an agent. Nothing here creates, changes or deletes a
// platform resource: a change to a box is a commit to its repository,
// reconciled by its own controllers (ADR-043, ADR-065), and an MCP server that
// applied changes would be a second authority over every domain it touched.
//
// It reads AS THE CALLER. The clients a tool receives are built per request by
// the host: impersonating the person whose token was validated (cmd/mcp-server),
// or with the operator's own kubeconfig (`kakt mcp`). So Kubernetes RBAC, not
// this package, decides what each call may see.
//
// The shape follows grafana/mcp-grafana: tools in categories that can each be
// disabled, read-only and destructive hints on every tool, and the Kubernetes
// permissions each tool needs declared in its metadata -- a test fails a tool
// that declares none.
package boxmcp

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"strings"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"k8s.io/client-go/discovery"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/kubernetes"
)

// Version is the server's reported version; the build sets it.
var Version = "development"

// RequiredPermissionsMetaKey lists, in a tool's _meta, the Kubernetes access
// its calls need ("verb group/resource"). A caller missing one gets a 403 from
// the API server, which the tool reports as such.
const RequiredPermissionsMetaKey = "kamarkat.io/requiredPermissions"

// Categories, each of which can be disabled.
const (
	CategoryBox      = "box"
	CategoryClusters = "clusters"
	CategoryGitops   = "gitops"
	CategoryPlatform = "platform"
	CategoryApps     = "apps"
	CategoryCerts    = "certs"
	CategoryBackups  = "backups"
	CategoryEnvs     = "envs"
	CategoryVerify   = "verify"
)

// AllCategories is every category this server knows, in the order tools list.
var AllCategories = []string{CategoryBox, CategoryClusters, CategoryGitops, CategoryPlatform, CategoryApps, CategoryCerts, CategoryBackups, CategoryEnvs, CategoryVerify}

// Clients is what a tool reads with: the caller's view of the cluster.
type Clients struct {
	Core      kubernetes.Interface
	Dynamic   dynamic.Interface
	Discovery discovery.DiscoveryInterface
}

// ClientsFor returns the clients for the request in ctx.
type ClientsFor func(ctx context.Context) (*Clients, error)

// Options configures a server.
type Options struct {
	// Disabled categories. `verify` is disabled unless EnableVerify is set,
	// because it is the one category that starts work in the box.
	Disabled     []string
	EnableVerify bool
	// Clients builds the caller's clients for each call.
	Clients ClientsFor
}

func (o Options) enabled(category string) bool {
	if category == CategoryVerify && !o.EnableVerify {
		return false
	}
	return !slices.Contains(o.Disabled, category)
}

// EnabledCategories is the categories a server built with o serves.
func (o Options) EnabledCategories() []string {
	var out []string
	for _, c := range AllCategories {
		if o.enabled(c) {
			out = append(out, c)
		}
	}
	return out
}

// tool is one registration: its category, its permissions, and how to add it.
type tool struct {
	category    string
	permissions []string
	readOnly    bool
	register    func(s *mcp.Server, t *mcp.Tool)
	def         mcp.Tool
}

// NewServer builds the server for one caller.
func NewServer(o Options) *mcp.Server {
	s := mcp.NewServer(&mcp.Implementation{Name: "kamarkat-mcp", Version: Version}, &mcp.ServerOptions{
		Instructions: "The state of this kamarkat box, read as you. Nothing here changes the box: " +
			"changes are pull requests on the box's gitops repository.",
	})
	for _, t := range tools(o.Clients) {
		if !o.enabled(t.category) {
			continue
		}
		def := t.def
		destructive := false
		def.Annotations = &mcp.ToolAnnotations{ReadOnlyHint: t.readOnly, DestructiveHint: &destructive}
		def.Meta = mcp.Meta{RequiredPermissionsMetaKey: t.permissions, "kamarkat.io/category": t.category}
		t.register(s, &def)
	}
	return s
}

// ToolNames is every tool a server built with o lists. For tests and docs.
func ToolNames(o Options) []string {
	var names []string
	for _, t := range tools(o.Clients) {
		if o.enabled(t.category) {
			names = append(names, t.def.Name)
		}
	}
	return names
}

// add registers a typed handler.
func add[In any](category string, readOnly bool, permissions []string, def mcp.Tool, h func(context.Context, In) (any, error)) tool {
	return tool{
		category:    category,
		permissions: permissions,
		readOnly:    readOnly,
		def:         def,
		register: func(s *mcp.Server, t *mcp.Tool) {
			mcp.AddTool(s, t, func(ctx context.Context, _ *mcp.CallToolRequest, in In) (*mcp.CallToolResult, any, error) {
				out, err := h(ctx, in)
				if err != nil {
					return &mcp.CallToolResult{IsError: true, Content: []mcp.Content{&mcp.TextContent{Text: explain(err)}}}, nil, nil
				}
				body, merr := json.MarshalIndent(out, "", "  ")
				if merr != nil {
					return nil, nil, fmt.Errorf("encode result: %w", merr)
				}
				return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: string(body)}}}, nil, nil
			})
		},
	}
}

// explain turns an API error into what an agent can act on. A 403 names the
// reason: the caller's role does not include it, which the person can change in
// the box and the agent must not work around.
func explain(err error) string {
	msg := err.Error()
	if strings.Contains(msg, "forbidden") {
		return "Forbidden: your access in this box does not include this (" + msg + "). " +
			"An administrator of the box can grant it; do not look for another way."
	}
	return msg
}
