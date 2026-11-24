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
	// SpokeIssuerRole, SpokeIssuerNamespace, SpokeIssuerServiceAccount: the spoke's
	// cert-manager Vault ClusterIssuer (openbao-fleet) logs in as this identity.
	SpokeIssuerRole           = "cert-manager"
	SpokeIssuerNamespace      = "cert-manager"
	SpokeIssuerServiceAccount = "openbao-issuer"
	// Spoke certificate lifetimes, the ceilings of their pki_int roles.
	spokeClientTTL = "24h"
	spokeCacheTTL  = "168h"
)

// SpokeClientIdentities are the only client identities a workload cluster may
// hold: its own Argo CD agent, Alloy and Support Agent, each named for it.
func SpokeClientIdentities(cluster string) []string {
	return []string{"argocd-agent:" + cluster, "alloy:" + cluster, "support-agent:" + cluster}
}

// SpokePKIRole signs this cluster's client identities; SpokeCachePKIRole its
// tenant cache servers (<app>-cache). Each cluster has its own, and its signing
// policy names only these two (ADR-035, ADR-117 §8).
func SpokePKIRole(cluster string) string      { return "spoke-" + cluster }
func SpokeCachePKIRole(cluster string) string { return "spoke-" + cluster + "-cache" }

// spokePKIRoles are the two roles' constraints. CN checking: OpenBao skips the
// allowed_domains check entirely when cn_validations is ["disabled"], so it is
// ["hostname"] -- which here only admits the CN type -- with enforce_hostnames off
// for the client identities, whose names carry a colon; allow_bare_domains is an
// exact, case-insensitive match. Nothing else is admitted: no IP, URI or other
// SANs, no wildcards, no localhost, and one key usage per role.
func spokePKIRoles(cluster string) map[string]map[string]any {
	common := map[string]any{
		"issuer_ref":                  "default",
		"allow_any_name":              false,
		"allow_subdomains":            false,
		"allow_localhost":             false,
		"allow_ip_sans":               false,
		"allowed_uri_sans":            "",
		"allowed_other_sans":          "",
		"allow_wildcard_certificates": false,
		"allow_token_displayname":     false,
		"require_cn":                  true,
		"use_csr_common_name":         true,
		"use_csr_sans":                true,
		"cn_validations":              []string{"hostname"},
		"key_usage":                   []string{"DigitalSignature", "KeyEncipherment"},
		"ext_key_usage":               []string{},
		"no_store":                    true,
	}
	merge := func(extra map[string]any) map[string]any {
		out := map[string]any{}
		for k, v := range common {
			out[k] = v
		}
		for k, v := range extra {
			out[k] = v
		}
		return out
	}
	return map[string]map[string]any{
		SpokePKIRole(cluster): merge(map[string]any{
			"allowed_domains":    SpokeClientIdentities(cluster),
			"allow_bare_domains": true,
			"allow_glob_domains": false,
			"enforce_hostnames":  false,
			"client_flag":        true,
			"server_flag":        false,
			"ttl":                spokeClientTTL,
			"max_ttl":            spokeClientTTL,
		}),
		SpokeCachePKIRole(cluster): merge(map[string]any{
			"allowed_domains":    []string{"*-cache"},
			"allow_bare_domains": false,
			"allow_glob_domains": true,
			"enforce_hostnames":  true,
			"client_flag":        false,
			"server_flag":        true,
			"ttl":                spokeCacheTTL,
			"max_ttl":            spokeCacheTTL,
		}),
	}
}

// SpokeMount is the auth mount for one workload cluster.
func SpokeMount(cluster string) string { return "spoke-" + cluster }

// SpokePolicyName, SpokePKIPolicyName and TenantPolicyName are UID-scoped.
func SpokePolicyName(uid string) string    { return "spoke-" + uid }
func SpokePKIPolicyName(uid string) string { return "spoke-pki-" + uid }
func TenantPolicyName(uid string) string   { return "tenant-" + uid }

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
	if err := c.putJWTRole(ctx, mount, SpokePlatformRole,
		subject(SpokePlatformNamespace, SpokePlatformServiceAccount), policy); err != nil {
		return err
	}
	// The spoke's certificate issuer: signing only, through this cluster's own
	// two roles, a separate identity and policy so the secret reader cannot sign
	// and the signer cannot read secrets or sign as any other cluster.
	for role, params := range spokePKIRoles(cluster) {
		if err := c.PutPKIRole(ctx, role, params); err != nil {
			return fmt.Errorf("pki_int role %s: %w", role, err)
		}
	}
	pkiPolicy := SpokePKIPolicyName(uid)
	if err := c.PutPolicy(ctx, pkiPolicy, fmt.Sprintf(
		"path \"pki_int/sign/%s\" { capabilities = [\"create\", \"update\"] }\n"+
			"path \"pki_int/sign/%s\" { capabilities = [\"create\", \"update\"] }\n",
		SpokePKIRole(cluster), SpokeCachePKIRole(cluster))); err != nil {
		return fmt.Errorf("write policy %s: %w", pkiPolicy, err)
	}
	return c.putJWTRole(ctx, mount, SpokeIssuerRole,
		subject(SpokeIssuerNamespace, SpokeIssuerServiceAccount), pkiPolicy)
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
	for _, p := range []string{SpokePolicyName(uid), SpokePKIPolicyName(uid)} {
		if err := c.DeletePolicy(ctx, p); err != nil {
			return err
		}
	}
	if err := c.DisableMount(ctx, SpokeMount(cluster)); err != nil {
		return err
	}
	for role := range spokePKIRoles(cluster) {
		if err := c.DeletePKIRole(ctx, role); err != nil {
			return err
		}
	}
	return c.DeleteTree(ctx, "spokes/"+cluster)
}
