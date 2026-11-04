package assets

import (
	"embed"
	"fmt"
	"time"
)

//go:embed manifests
var manifestsFS embed.FS

//go:embed catalog
var catalogFS embed.FS

// ReadManifest reads a manifest file from the embedded manifests directory
func ReadManifest(path string) ([]byte, error) {
	return manifestsFS.ReadFile("manifests/" + path)
}

// ReadCatalog reads a catalog file from the embedded catalog directory
func ReadCatalog(path string) ([]byte, error) {
	return catalogFS.ReadFile("catalog/" + path)
}

// EncryptionSecretName is the Secret a control plane reads its at-rest encryption
// provider configuration from, for one cluster.
//
// IT EXISTS SO THERE IS EXACTLY ONE SPELLING. The name appears in three places that
// cannot see each other: this file, the Day-0 Secret template rendered by the
// provisioner and by `kakt encryption enable`, and the `secretEncryptionConfig`
// patch in each ClusterClass, which builds it from `{{ .builtin.cluster.name }}`
// inside CAPI at topology-reconcile time. A mismatch between the Go half and the
// CAPI half produces a control-plane node referencing a Secret that does not exist,
// and CAPI reports that as a bootstrap that never completes rather than as a name it
// could not find. Preflight 003-encryption-secret-is-per-cluster asserts the two halves agree.
//
// The key is per cluster and the escrow holds it under the cluster's id (ADR-076).
// An earlier version named one flat Secret for every cluster of a class, which is
// the defect this function's existence prevents.
func EncryptionSecretName(cluster string) string {
	return cluster + "-encryption-config"
}

// EncryptionKeyName is the name a given generation of the at-rest encryption key
// carries inside the provider configuration.
//
// IT IS NOT COSMETIC: the API server writes the key's NAME into every stored object,
// as `k8s:enc:secretbox:v1:<name>:<nonce><ciphertext>`. So the name in etcd says which
// generation sealed each Secret, and counting objects that still carry the previous
// name is how "every Secret has been rewritten under the new key" becomes a measured
// fact rather than an assumption.
//
// A single fixed name would make two generations indistinguishable in etcd, which
// would leave a rotation performed but unprovable — and after a key disclosure,
// unprovable is the same as unfinished.
//
// Generations start at 1 because the key store convention elsewhere in this platform
// does, and because 0 is what an unread value looks like.
func EncryptionKeyName(generation int) string {
	return fmt.Sprintf("key%d", generation)
}

// EncryptionRotationWindow is how often an at-rest encryption key is replaced.
//
// NINETY DAYS, AND IT IS ONE NUMBER DELIBERATELY.
//
// WHAT "THE ENTERPRISE STANDARD" ACTUALLY SAYS, because it is worth not overstating:
//
//   - PCI DSS v4.0 req. 3.7.4 requires a cryptoperiod to be DEFINED and keys rotated at
//     the end of it. It names no number. The requirement is that somebody decided.
//   - NIST SP 800-57 Part 1 puts the originator-usage period of a symmetric
//     data-encryption key at up to two years. That is a ceiling, not a target.
//   - Cloud-provider convention is the tighter figure: Google's guidance for a
//     symmetric encryption key is 90 days; AWS automatic rotation is annual.
//
// 90 days is the tighter convention and the figure this platform uses.
//
// A DISCLOSED KEY IS NOT A CADENCE MATTER, and this constant must not be read as
// permission to wait. A key known to have left its trust boundary is an incident: it is
// replaced now, and the 90-day clock is what governs keys that have not. Treating a
// disclosure as "due at the next window" is how a known-compromised key stays in force
// for a quarter.
const EncryptionRotationWindow = 90 * 24 * time.Hour

// EncryptionRotationWindowDays renders the window the way an operator reads it.
//
// time.Duration formats 90 days as "2160h0m0s", which is not a cryptoperiod anybody
// recognises in help text or in a runbook.
func EncryptionRotationWindowDays() int {
	return int(EncryptionRotationWindow.Hours() / 24)
}
