package openbao

import (
	"context"
	"fmt"
	"strings"
)

// Spoke and tenant authorization (ADR-117 §4).
//
// Every workload cluster gets its own JWT auth mount, auth/spoke-<cluster>, that
// trusts exactly one key: the public half of that cluster's ServiceAccount
// signing key (<cluster>-sa tls.crt, written by Cluster API). Nothing else about
// the spoke is trusted and nothing is fetched from it, so a spoke that is down or
// compromised cannot widen what another spoke reads.
//
// Roles on that mount bind one ServiceAccount each, by subject, with the audience
// "openbao". Policies are named by the owning object's UID, so a deleted and
// recreated object of the same name never inherits the old one's authority, and
// deleting the policy withdraws it on the very next request of any token holding
// it (ACLs are resolved per request).
const (
	// Audience every spoke and tenant token is minted for.
	Audience = "openbao"
	// SpokeTokenTTL bounds every token a spoke mount issues.
	SpokeTokenTTL = "10m"
	// SpokeIssuer is the kubeadm default service-account issuer.
	SpokeIssuer = "https://kubernetes.default.svc.cluster.local"

	// SpokePlatformNamespace / SpokePlatformServiceAccount: the spoke's ESO
	// identity for its ClusterSecretStore openbao-platform.
	SpokePlatformNamespace      = "external-secrets"
	SpokePlatformServiceAccount = "openbao-platform-reader"
	// TenantServiceAccount is the identity each tenant namespace's SecretStore uses.
	TenantServiceAccount = "openbao-secrets"
	// SpokePlatformRole is the role name on each spoke mount.
	SpokePlatformRole = "platform"
)

// SpokeMount is the auth mount for one workload cluster.
func SpokeMount(cluster string) string { return "spoke-" + cluster }

// SpokePolicyName and TenantPolicyName are UID-scoped.
func SpokePolicyName(uid string) string  { return "spoke-" + uid }
func TenantPolicyName(uid string) string { return "tenant-" + uid }

// TenantRole is the role on the cluster's mount for one application.
func TenantRole(app string) string { return "tenant-" + app }

// TenantNamespace mirrors universal-tenant.namespace.
func TenantNamespace(tenantID, app string) string { return "tenant-" + tenantID + "-" + app }

func subject(ns, sa string) string { return "system:serviceaccount:" + ns + ":" + sa }

// EnsureSpokeAuth converges cluster's mount, its key, its platform policy and role.
func (c *Client) EnsureSpokeAuth(ctx context.Context, cluster, uid, signingKeyPEM string) error {
	if strings.TrimSpace(signingKeyPEM) == "" {
		return fmt.Errorf("no ServiceAccount signing key for %s", cluster)
	}
	mount := SpokeMount(cluster)
	if err := c.EnsureJWTMount(ctx, mount, SpokeTokenTTL); err != nil {
		return err
	}
	if err := c.Write(ctx, "auth/"+mount+"/config", map[string]any{
		"jwt_validation_pubkeys": []string{strings.TrimSpace(signingKeyPEM)},
		"bound_issuer":           SpokeIssuer,
	}, nil); err != nil {
		return fmt.Errorf("configure auth/%s: %w", mount, err)
	}
	policy := SpokePolicyName(uid)
	if err := c.PutPolicy(ctx, policy, fmt.Sprintf(
		"path \"kv/data/%s\" { capabilities = [\"read\"] }\n", SpokePlatformScope(cluster))); err != nil {
		return fmt.Errorf("write policy %s: %w", policy, err)
	}
	return c.putJWTRole(ctx, mount, SpokePlatformRole,
		subject(SpokePlatformNamespace, SpokePlatformServiceAccount), policy)
}

// EnsureTenantAuth converges one application's role and policy on its cluster's mount.
func (c *Client) EnsureTenantAuth(ctx context.Context, cluster, tenantID, app, uid string) error {
	policy := TenantPolicyName(uid)
	if err := c.PutPolicy(ctx, policy, fmt.Sprintf(
		"path \"kv/data/%s\" { capabilities = [\"read\"] }\n", TenantScope(cluster, app))); err != nil {
		return fmt.Errorf("write policy %s: %w", policy, err)
	}
	return c.putJWTRole(ctx, SpokeMount(cluster), TenantRole(app),
		subject(TenantNamespace(tenantID, app), TenantServiceAccount), policy)
}

func (c *Client) putJWTRole(ctx context.Context, mount, role, sub, policy string) error {
	if err := c.Write(ctx, "auth/"+mount+"/role/"+role, map[string]any{
		"role_type":       "jwt",
		"user_claim":      "sub",
		"bound_subject":   sub,
		"bound_audiences": []string{Audience},
		"token_policies":  []string{policy},
		"token_type":      "service",
		"token_ttl":       SpokeTokenTTL,
		"token_max_ttl":   SpokeTokenTTL,
		// No token outlives its maximum, whatever renews it.
		"token_explicit_max_ttl": SpokeTokenTTL,
	}, nil); err != nil {
		return fmt.Errorf("write auth/%s/role/%s: %w", mount, role, err)
	}
	return nil
}

// RemoveTenantAuth withdraws one application's authority: its policy first, so a
// token already issued stops working on its next request, then its role.
func (c *Client) RemoveTenantAuth(ctx context.Context, cluster, app, uid string) error {
	if err := c.DeletePolicy(ctx, TenantPolicyName(uid)); err != nil {
		return err
	}
	return c.Delete(ctx, "auth/"+SpokeMount(cluster)+"/role/"+TenantRole(app))
}

// RemoveSpoke withdraws a cluster entirely: its policy, its mount (which revokes
// every token the mount issued) and its KV tree.
func (c *Client) RemoveSpoke(ctx context.Context, cluster, uid string) error {
	if err := c.DeletePolicy(ctx, SpokePolicyName(uid)); err != nil {
		return err
	}
	if err := c.DisableMount(ctx, SpokeMount(cluster)); err != nil {
		return err
	}
	return c.DeleteTree(ctx, "spokes/"+cluster)
}
