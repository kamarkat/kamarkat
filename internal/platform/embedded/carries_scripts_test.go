package embedded

import "testing"

// The closure is staged on disk by the generator and compiled in by the
// directive it also writes, and those are two different things: a run that
// copied every script while emitting `//go:embed all:manifests` produced a
// binary carrying none of them, and a check that looked at the filesystem
// passed. This reads the FS the binary actually ships.
//
// The entry points only. scripts/validate/runtime-script-closure.py proves the
// whole closure is staged; this proves the staging reaches the artefact.
func TestEmbeddedCarriesTheDayZeroEntryPoints(t *testing.T) {
	for _, p := range []string{
		"scripts/hub-bootstrap.sh",
		"scripts/validate-all.sh",
		"scripts/validate/run.sh",
		"manifests/hub-core-services/gateway/templates/hub-gateway.yaml",
	} {
		b, err := FS.ReadFile(p)
		if err != nil {
			t.Errorf("a released CLI carries no %s, so Day-0 cannot run: %v\n"+
				"Run scripts/package/embed-platform-assets.sh", p, err)
			continue
		}
		if len(b) == 0 {
			t.Errorf("%s is embedded and empty", p)
		}
	}
}

// go:embed records no file mode. Asserted rather than assumed, because the
// extractor restores the executable bit and a future change that made the FS
// mode meaningful would make that chmod look redundant.
func TestEmbeddedScriptsAreNotExecutable(t *testing.T) {
	f, err := FS.Open("scripts/hub-bootstrap.sh")
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer f.Close()
	fi, err := f.Stat()
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if fi.Mode()&0o111 != 0 {
		t.Fatalf("embed.FS reports mode %v, which has an executable bit: the "+
			"extractor's chmod may no longer be required -- check before removing it",
			fi.Mode())
	}
}
