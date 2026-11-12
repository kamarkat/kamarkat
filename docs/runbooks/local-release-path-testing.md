# Testing the released path locally

Bootstrap a cluster from your laptop using the exact path a tenant's Day-0 takes:
platform content read from the binary's embedded tree, charts pulled from GHCR by
version, bundle rendered in the published-chart shape.

This is not the same as `make build` and running `./kakt`. That produces a
**development** build, which ADR-068 sends to the working tree for its manifests and
which renders a bundle pointing ArgoCD at this repository at a branch. A tenant runs
neither. Two defects reached clusters through that gap — see ADR-068 addendum 1.

Use the development build for ordinary work. Use this before a release, and whenever
a change touches what a released binary carries or what `tenant scaffold` renders.

---

## The short version

```bash
make e2e VERSION=0.1.16-rc.1 DRY=1     # package and gate, publish nothing
make e2e VERSION=0.1.16-rc.1           # the whole loop
```

`scripts/dev/local-e2e.sh` runs all four phases below and reads every credential
from `k8-secrets/`, so nothing is prompted. Re-run one phase by naming it:

```bash
./scripts/dev/local-e2e.sh 0.1.16-rc.1 cli bootstrap
```

Settings are environment overrides — `TENANT`, `GIT_ORG`, `DOMAIN`, `CLUSTER`,
`ENVIRONMENT`, `PROVIDER`, `REGION`, `OWNER`, `WORKSPACE`. The credentials it
reads, one file per value:

| file | used as |
|---|---|
| `k8-secrets/hetzner/token` | `--provider-token`, `HCLOUD_TOKEN` |
| `k8-secrets/github/github-pat-token` | `--gitops-token` |
| `k8-secrets/infisical/INFISICAL_ESCROW_PROJECT_ID` | `--escrow-project-id` |
| `k8-secrets/infisical/INFISICAL_ESCROW_CLIENT_ID` | `--escrow-client-id` |
| `k8-secrets/infisical/INFISICAL_ESCROW_CLIENT_SECRET` | `--escrow-client-secret` |
| `k8-secrets/infisical/INFISICAL_ESCROW_URL` | `--escrow-url` (defaults to `https://app.infisical.com`) |
| `k8-secrets/s3/*` | `S3_*`, CNPG Barman backups of the platform database. A **destination**: absent, the box bootstraps and runs with no backups, and the run says so. |
| `k8-secrets/grafana-cloud/irm-*` | `GRAFANA_IRM_*`, where this box's alerts page (ADR-078 add.5 §5). A destination, same rule. |
| `k8-secrets/ghcr/username`, `k8-secrets/ghcr/token` | `GHCR_USERNAME`/`GHCR_TOKEN`, for pulling the tenant's private images (ADR-066). Optional here: falls back to the GitHub PAT above, which carries `write:packages`, with the login from `gh`. A real tenant supplies a token scoped to their own images. |
| `k8-secrets/tailscale/authkey` | `--tailscale-authkey`, hybrid only |

They are loaded before anything is published, so a missing one stops the run
rather than spending a version on a run that cannot finish.

The rest of this page is what those phases do, for when one of them fails.

---

## 1. Publish a prerelease

Authenticate helm once per session — the script does not log in, because the
credential is yours:

```bash
echo "$CHART_PUBLISH_TOKEN" | helm registry login ghcr.io -u <your-gh-user> --password-stdin
```

Then package, gate and publish:

```bash
make publish-local VERSION=0.1.16-rc.1
```

This runs `scripts/package/publish.sh` — the same script the release workflow
runs, so what lands in the registry passed the same gates a release passes:
bundle portability, chart-renders-what-the-cluster-applies, Applications
deployable, and the support matrix.

The `-rc.N` suffix is what keeps this cheap. ADR-063 consumes a version by
publishing it, so a version used for testing can never be released; `0.1.16-rc.1`
is a distinct version, so `0.1.16` stays free for the set you actually release.
Iterate with `-rc.2`, `-rc.3`.

Charts land in `oci://ghcr.io/kamarkat/charts` — the same registry a release uses,
so the `repoURL` your test renders is identical to a tenant's. `OWNER=` overrides
the namespace, but it must match what the scaffolded bundle names.

**To check packaging without spending a version:**

```bash
make publish-local VERSION=0.1.16-rc.1 SKIP_PUSH=1
```

Everything runs except the push. This is how you find a packaging mistake before
committing to an `-rc` number.

**When to use CI instead.** `gh workflow run publish-platform-charts.yml -f
version=0.1.16-rc.1` does the same thing on a clean runner, from the pushed ref
rather than your working tree. It takes about 9m30s. Prefer it when you want to
confirm the change works from what is actually committed — and always for a real
release, which is a tag, not a dispatch.

## 2. Get a binary that declares that version

Either works. Pick by what you are testing.

**Testing manifests or charts** — take the bytes CI built, so a failure cannot be
your toolchain (requires a CI run, not a local publish):

```bash
make cli-fetch VERSION=0.1.16-rc.1
```

**Testing a change to the CLI itself, or pairing with a local publish** — build it,
same recipe CI uses (`scripts/package/build-cli.sh`):

```bash
make cli-release VERSION=0.1.16-rc.1
```

Either way, confirm before going further. A binary reporting `development` here
puts you back on the path you are trying to leave, and nothing downstream will say
so:

```bash
bin/kakt bundle-version   # must print 0.1.16-rc.1
```

## 3. Scaffold against it

```bash
export PATH="$PWD/bin:$PATH"
export HCLOUD_TOKEN=$(cat k8-secrets/hetzner/token)

kakt tenant scaffold --local \
  --tenant acme --box <your-gh-org> --domain acme.example \
  --cluster acme-hub --environment dev --provider hetzner \
  --bundle-version 0.1.16-rc.1
```

`--bundle-version` is redundant when the binary already declares it — it defaults to
the version the CLI carries — but passing it makes the intent visible in shell
history, which matters when several prereleases are in flight.

Scaffolding prompts for the tenant's credentials, including the Infisical escrow
(ADR-076). `--local` clones the repository it creates and prints the bootstrap
command instead of dispatching the workflow.

**Check the rendered bundle before bootstrapping.** This is the assertion the whole
loop exists to make:

```bash
grep -A3 repoURL acme-gitops/registry/clusters/acme/bundle.yaml
```

Expect `ghcr.io/kamarkat/charts`, a `chart:` key, and a `bundleVersion` parameter
matching `targetRevision`. If you see a `github.com` URL with a `path:` key, the
binary is a development build — go back to step 2.

## 4. Bootstrap

Run it from the clone, exactly as the workflow does:

```bash
cd acme-gitops
kakt bootstrap \
  --name acme-hub --provider hetzner --region hel1 \
  --environment dev --gitops-dir .
```

Working from the tenant repository is correct **only** with a released binary — its
platform content comes from the embedded tree, so the working directory is irrelevant.
A development build resolves `manifests/…` against the cwd and fails here with a
`no such file or directory` naming a file this repository plainly has.

`HCLOUD_TOKEN` is exported in step 3 because the `k8-secrets/hetzner/token` fallback
is resolved against the platform checkout, and you are no longer in it.

## 5. Tear down

```bash
cd ..
kakt teardown --name acme-hub --force --confirm
```

Then delete the scaffolded repository and its local clone. `kakt tenant scaffold`
refuses to clone over an existing directory, so a stale clone will stop the next run
rather than being silently bootstrapped.

---

## What this costs

**A manifest change needs a new prerelease.** The cluster reconciles the charts that
were published, not your working tree. Editing
`manifests/argocd/environment-manager/…` and re-running step 4 changes nothing, and
the run looks entirely normal while testing the previous content. Publish `-rc.2`.

A change to the **CLI** does not — rebuild with `make cli-release` at the same version
and re-run. The CLI and the charts are separate artifacts.

Roughly, per iteration:

| change | what to re-run |
|---|---|
| CLI code | `make cli-release VERSION=<same rc>` |
| manifest / chart | `make publish-local VERSION=<next rc>` then `make cli-release` |
| neither, re-testing | nothing; re-run step 4 |

This is the trade. The development build exists because requiring a publish per
iteration makes iteration slow; this path exists because the development build does
not test what ships. Use the first while building, the second before releasing.

## What is guarded without running this

Two checks make the older failure mode a red pull request rather than a tenant's
first bootstrap:

- `internal/kakt-cli/bootstrap/embedded_assets_test.go` — asserts a released binary
  carries every path Day-0 reads, per provider, environment and boundary. Add a Day-0
  read of a manifest outside `PATHS` in `scripts/package/embed-platform-assets.sh` and
  this fails.
- `.github/workflows/pr-validate.yaml` runs the embed drift hook and the CLI's tests.
  Both were previously enforced only by a pre-commit hook on one machine.

Neither replaces this runbook: they check that the binary *carries* what it reads,
not that what it renders *reconciles*.

## References

- ADR-063: The Platform Bundle and its Version — a version is consumed by publishing
- ADR-068 addendum 1: a prerelease is how the released path is exercised
- ADR-072: tenant-controlled Day-0
- `docs/runbooks/bootstrap-and-binaries.md` — the development-build loop
