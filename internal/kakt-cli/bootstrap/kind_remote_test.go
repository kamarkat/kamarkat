package bootstrap

import "testing"

func TestNormalizeDockerEndpoint(t *testing.T) {
	cases := []struct {
		in   string
		want string
	}{
		{"ssh://Dell@192.168.1.18", "192.168.1.18"},
		{"ssh://192.168.1.18", "192.168.1.18"},
		{"ssh://Dell@192.168.1.18:22", "192.168.1.18"},
		{"tcp://192.168.1.18:2376", "192.168.1.18:2376"},
		{"tcp://192.168.1.18", "192.168.1.18"},
		{"unix:///var/run/docker.sock", ""},
		{"", ""},
	}
	for _, c := range cases {
		if got := normalizeDockerEndpoint(c.in); got != c.want {
			t.Errorf("normalizeDockerEndpoint(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}
