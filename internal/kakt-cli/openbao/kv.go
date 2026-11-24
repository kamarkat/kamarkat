package openbao

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"

	"k8s.io/client-go/kubernetes"
)

const (
	// The administrator's identity for `kakt app secrets` (ADR-117 §4): it may read
	// and write tenant application secrets and nothing else. Tokens for its
	// ServiceAccount can be minted only by cluster administrators.
	roleAppSecrets = "openbao-app-secrets"
	saAppSecrets   = "openbao-app-secrets"
)

// ConnectForAppSecrets opens the box's OpenBao as the app-secrets identity.
func ConnectForAppSecrets(ctx context.Context, cs kubernetes.Interface, kubeconfig string) (*Client, error) {
	c, err := Connect(ctx, cs, kubeconfig)
	if err != nil {
		return nil, err
	}
	if err := c.LoginAs(ctx, cs, saAppSecrets, roleAppSecrets); err != nil {
		c.Close()
		return nil, err
	}
	return c, nil
}

type kvRead struct {
	Data struct {
		Data     map[string]string `json:"data"`
		Metadata struct {
			Version int `json:"version"`
		} `json:"metadata"`
	} `json:"data"`
}

// KVKeys returns the key NAMES held at a KV v2 path, never their values. An
// absent secret is an empty list, not an error.
func KVKeys(ctx context.Context, c *Client, path string) ([]string, error) {
	var r kvRead
	err := c.Read(ctx, "kv/data/"+strings.Trim(path, "/"), &r)
	if errors.Is(err, ErrNotFound) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	keys := make([]string, 0, len(r.Data.Data))
	for k := range r.Data.Data {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys, nil
}

// KVMerge writes values into the KV v2 secret at path, keeping keys it does not
// name. Check-and-set on the version read, so two concurrent writers cannot
// silently overwrite each other: the loser fails and is re-run.
func KVMerge(ctx context.Context, c *Client, path string, values map[string]string) error {
	path = strings.Trim(path, "/")
	var r kvRead
	err := c.Read(ctx, "kv/data/"+path, &r)
	if err != nil && !errors.Is(err, ErrNotFound) {
		return err
	}
	merged := map[string]string{}
	for k, v := range r.Data.Data {
		merged[k] = v
	}
	for k, v := range values {
		if strings.TrimSpace(v) == "" {
			return fmt.Errorf("refusing to write an empty value for %s", k)
		}
		merged[k] = v
	}
	return c.Write(ctx, "kv/data/"+path, map[string]any{
		"data":    merged,
		"options": map[string]any{"cas": r.Data.Metadata.Version},
	}, nil)
}
