package bootstrap

import (
	"os"
	"path/filepath"
	"testing"
)

// kind writes a new cluster's context into $KUBECONFIG when it is set, and this
// resolver did not read it -- so every kubectl call looked in ~/.kube/config for
// a context kind had written elsewhere. The run died three phases later on
// `dial tcp [::1]:8080`, naming neither the file nor the variable.
func TestDefaultKubeconfigPathHonoursEnv(t *testing.T) {
	home, _ := os.UserHomeDir()
	fallback := filepath.Join(home, ".kube", "config")

	for _, c := range []struct {
		name, env, want string
	}{
		{"unset", "", fallback},
		{"blank", "   ", fallback},
		{"single", "/tmp/a.kubeconfig", "/tmp/a.kubeconfig"},
		// The first entry, which is where kubectl writes and therefore where
		// kind put the context. Reading a later one is the same bug again.
		{"list", "/tmp/a.kubeconfig" + string(os.PathListSeparator) + "/tmp/b.kubeconfig", "/tmp/a.kubeconfig"},
	} {
		t.Run(c.name, func(t *testing.T) {
			t.Setenv("KUBECONFIG", c.env)
			if got := defaultKubeconfigPath(); got != c.want {
				t.Errorf("KUBECONFIG=%q: got %q, want %q", c.env, got, c.want)
			}
		})
	}
}
