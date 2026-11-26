package openbao

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
)

// KV layout (ADR-117): one KV v2 secret per scope, each value a key in it --
// the shape the Infisical folders had, so an ExternalSecret reads
// remoteRef.key = <scope>, property = <key>:
//
//	hub                          the hub's platform and application secrets
//	spokes/<cluster>/platform    one workload cluster's platform credentials
//	spokes/<cluster>/tenants/<app>  one tenant application's secrets
const (
	ScopeHub = "hub"
)

// SpokePlatformScope and TenantScope name the per-cluster scopes.
func SpokePlatformScope(cluster string) string { return "spokes/" + cluster + "/platform" }
func TenantScope(cluster, app string) string   { return "spokes/" + cluster + "/tenants/" + app }

type kvDoc struct {
	Data struct {
		Data     map[string]any `json:"data"`
		Metadata struct {
			Version int `json:"version"`
		} `json:"metadata"`
	} `json:"data"`
}

func (c *Client) readScope(ctx context.Context, scope string) (map[string]any, int, error) {
	var r kvDoc
	err := c.Read(ctx, "kv/data/"+strings.Trim(scope, "/"), &r)
	if errors.Is(err, ErrNotFound) {
		return map[string]any{}, 0, nil
	}
	if err != nil {
		return nil, 0, err
	}
	if r.Data.Data == nil {
		r.Data.Data = map[string]any{}
	}
	return r.Data.Data, r.Data.Metadata.Version, nil
}

// ReadScope returns every value in a scope as strings.
func (c *Client) ReadScope(ctx context.Context, scope string) (map[string]string, error) {
	data, _, err := c.readScope(ctx, scope)
	if err != nil {
		return nil, err
	}
	out := make(map[string]string, len(data))
	for k, v := range data {
		if s, ok := v.(string); ok {
			out[k] = s
		}
	}
	return out, nil
}

// GetSecret returns scope's value for key; ErrNotFound when absent.
func (c *Client) GetSecret(ctx context.Context, scope, key string) (string, error) {
	data, _, err := c.readScope(ctx, scope)
	if err != nil {
		return "", err
	}
	v, ok := data[key].(string)
	if !ok || v == "" {
		return "", ErrNotFound
	}
	return v, nil
}

// SecretExists distinguishes absent from unreadable.
func (c *Client) SecretExists(ctx context.Context, scope, key string) (bool, error) {
	_, err := c.GetSecret(ctx, scope, key)
	switch {
	case err == nil:
		return true, nil
	case errors.Is(err, ErrNotFound):
		return false, nil
	default:
		return false, err
	}
}

// ErrExists is CreateSecret finding a value already there.
var ErrExists = errors.New("already exists")

// mutate applies fn to the scope under check-and-set, retrying a lost race: two
// controllers writing different keys of one scope must both land.
func (c *Client) mutate(ctx context.Context, scope string, fn func(map[string]any) (bool, error)) error {
	scope = strings.Trim(scope, "/")
	for attempt := 0; ; attempt++ {
		data, version, err := c.readScope(ctx, scope)
		if err != nil {
			return err
		}
		changed, err := fn(data)
		if err != nil || !changed {
			return err
		}
		err = c.Write(ctx, "kv/data/"+scope, map[string]any{
			"data": data, "options": map[string]any{"cas": version},
		}, nil)
		var ae *APIError
		if errors.As(err, &ae) && ae.Status == 400 && strings.Contains(ae.Body, "check-and-set") && attempt < 5 {
			continue
		}
		return err
	}
}

// CreateSecret adds key to scope only if absent: a generator must never
// overwrite a value that may already be in use (ADR-003 §1). ErrExists when present.
func (c *Client) CreateSecret(ctx context.Context, scope, key, value string) error {
	if value == "" {
		return fmt.Errorf("refusing to write an empty value for %s/%s", scope, key)
	}
	return c.mutate(ctx, scope, func(d map[string]any) (bool, error) {
		if v, ok := d[key].(string); ok && v != "" {
			return false, ErrExists
		}
		d[key] = value
		return true, nil
	})
}

// UpdateSecret sets key unconditionally (an authority converging a copy).
func (c *Client) UpdateSecret(ctx context.Context, scope, key, value string) error {
	if value == "" {
		return fmt.Errorf("refusing to write an empty value for %s/%s", scope, key)
	}
	return c.mutate(ctx, scope, func(d map[string]any) (bool, error) {
		if v, ok := d[key].(string); ok && v == value {
			return false, nil
		}
		d[key] = value
		return true, nil
	})
}

// DeleteSecret removes key from scope.
func (c *Client) DeleteSecret(ctx context.Context, scope, key string) error {
	return c.mutate(ctx, scope, func(d map[string]any) (bool, error) {
		if _, ok := d[key]; !ok {
			return false, nil
		}
		delete(d, key)
		return true, nil
	})
}

// DeleteScope removes a scope and every version of it.
func (c *Client) DeleteScope(ctx context.Context, scope string) error {
	return c.Delete(ctx, "kv/metadata/"+strings.Trim(scope, "/"))
}

// ListScopes lists the names directly under a path ("a/" for a sub-path).
func (c *Client) ListScopes(ctx context.Context, path string) ([]string, error) {
	var r struct {
		Data struct {
			Keys []string `json:"keys"`
		} `json:"data"`
	}
	err := c.List(ctx, "kv/metadata/"+strings.Trim(path, "/"), &r)
	if errors.Is(err, ErrNotFound) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	sort.Strings(r.Data.Keys)
	return r.Data.Keys, nil
}

// DeleteTree removes every scope under a path, recursively (cluster removal).
func (c *Client) DeleteTree(ctx context.Context, path string) error {
	path = strings.Trim(path, "/")
	keys, err := c.ListScopes(ctx, path)
	if err != nil {
		return err
	}
	for _, k := range keys {
		child := path + "/" + strings.TrimSuffix(k, "/")
		if strings.HasSuffix(k, "/") {
			if err := c.DeleteTree(ctx, child); err != nil {
				return err
			}
			continue
		}
		if err := c.DeleteScope(ctx, child); err != nil {
			return err
		}
	}
	return nil
}

// PutPolicy writes an ACL policy.
func (c *Client) PutPolicy(ctx context.Context, name, hcl string) error {
	return c.Write(ctx, "sys/policies/acl/"+name, map[string]any{"policy": hcl}, nil)
}

// DeletePolicy removes a policy. Policies are resolved by name on every request,
// so a token carrying a deleted policy loses that authority on its next request.
func (c *Client) DeletePolicy(ctx context.Context, name string) error {
	return c.Delete(ctx, "sys/policies/acl/"+name)
}

// ListPolicies lists ACL policy names.
func (c *Client) ListPolicies(ctx context.Context) ([]string, error) {
	var r struct {
		Data struct {
			Keys []string `json:"keys"`
		} `json:"data"`
	}
	if err := c.List(ctx, "sys/policies/acl", &r); err != nil && !errors.Is(err, ErrNotFound) {
		return nil, err
	}
	return r.Data.Keys, nil
}

// AuthMounts returns the enabled auth mount paths ("spoke-x/").
func (c *Client) AuthMounts(ctx context.Context) (map[string]string, error) {
	var r struct {
		Data map[string]struct {
			Type string `json:"type"`
		} `json:"data"`
	}
	if err := c.Read(ctx, "sys/auth", &r); err != nil {
		return nil, err
	}
	out := map[string]string{}
	for path, m := range r.Data {
		out[strings.TrimSuffix(path, "/")] = m.Type
	}
	return out, nil
}

// EnsureJWTMount enables auth/<mount> as a JWT mount if absent, tuned so no token
// it issues outlives maxTTL (ADR-117 §4).
func (c *Client) EnsureJWTMount(ctx context.Context, mount, maxTTL string) error {
	mounts, err := c.AuthMounts(ctx)
	if err != nil {
		return err
	}
	if _, ok := mounts[mount]; !ok {
		if err := c.Write(ctx, "sys/auth/"+mount, map[string]any{
			"type": "jwt", "config": map[string]any{"max_lease_ttl": maxTTL, "default_lease_ttl": maxTTL},
		}, nil); err != nil {
			return fmt.Errorf("enable auth/%s: %w", mount, err)
		}
	}
	return c.Write(ctx, "sys/auth/"+mount+"/tune", map[string]any{
		"max_lease_ttl": maxTTL, "default_lease_ttl": maxTTL,
	}, nil)
}

// DisableMount disables an auth mount, which revokes every token it issued.
func (c *Client) DisableMount(ctx context.Context, mount string) error {
	return c.Delete(ctx, "sys/auth/"+mount)
}

// PutPKIRole writes a pki_int role.
func (c *Client) PutPKIRole(ctx context.Context, name string, role map[string]any) error {
	return c.Write(ctx, "pki_int/roles/"+name, role, nil)
}

// DeletePKIRole removes a pki_int role.
func (c *Client) DeletePKIRole(ctx context.Context, name string) error {
	return c.Delete(ctx, "pki_int/roles/"+name)
}

// StaticRole is what database/static-roles/<name> reports.
type StaticRole struct {
	Username          string `json:"username"`
	LastVaultRotation string `json:"last_vault_rotation"`
	RotationPeriod    int    `json:"rotation_period"`
}

// ReadStaticRole reads a static role; ErrNotFound when absent.
func (c *Client) ReadStaticRole(ctx context.Context, name string) (StaticRole, error) {
	var r struct {
		Data StaticRole `json:"data"`
	}
	err := c.Read(ctx, "database/static-roles/"+name, &r)
	return r.Data, err
}

// GeneratePassword asks OpenBao for a value from a named password policy
// (sys/policies/password/<policy>/generate). OpenBao, not hub-operator, is the
// generator (ADR-117 §7.4): hub-operator only decides that a value is needed and
// writes the answer once into KV.
func (c *Client) GeneratePassword(ctx context.Context, policy string) (string, error) {
	var r struct {
		Data struct {
			Password string `json:"password"`
		} `json:"data"`
	}
	if err := c.Read(ctx, "sys/policies/password/"+policy+"/generate", &r); err != nil {
		return "", fmt.Errorf("generate from password policy %s: %w", policy, err)
	}
	if r.Data.Password == "" {
		return "", fmt.Errorf("password policy %s returned nothing", policy)
	}
	return r.Data.Password, nil
}
