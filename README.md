# Zero-Ops — StartupOS- Enterprise infrastructure for growing companies.

**We maintain the platform your team builds on.**

Zero-Ops is a complete, open-source infrastructure stack for software startups —
compute and cluster lifecycle, GitOps, identity, secrets, databases, gateway,
observability — packaged as one tested unit that runs on affordable
infrastructure instead of a hyperscaler.

The software is free. What is sold is maintenance: keeping the stack current,
patched and upgradeable without breaking what is running on it.

## The Platform

It helps small and medium scale brands to experience a enterprise grade platform which they cannot afford to have such huge investment for having a platform of their own to defining their working model that is unique for them to make a difference in their market. Save runway for startups, focus companies to build their capabilities inward within their org instead of relying on others. Giving them full control of their business. don’t separate software and scale as two separate concerns. Opensource has to move beyond, proving single tenant OSS open and gating the scale. We believe the diretcion of OSS must be improving the value it provides and not gating by scale..

## Who it is for

"You shouldn't have to become large before you deserve a serious platform."

Funded startups saving runway from hyperscaler pricing, who want a golden path
defined and maintained for them, and cannot staff a platform engineering team to
do it. Zero-Ops is platform engineering as a service: the golden path arrives
tested, and stays maintained, without the team it would otherwise take.

This is the opposite end of the market from tools that hand a platform team a
faster start and room to customise. A tenant here inherits a tested set rather
than a starting point to diverge from — see `docs/adr/071-how-this-platform-differs-from-kubefirst.md`.

## The box

A customer is a **tenant** and holds one **box**: a control plane plus that
tenant's own spoke clusters.

- **Bring your own cloud.** Clusters run in the tenant's account, on the
  tenant's bill. Zero-Ops resells no compute.
- **The control plane ships into the box.** It runs in the tenant's cloud under
  credentials the tenant holds. Zero-Ops operates no tenant infrastructure and
  holds no cloud, cluster or secret credential.
- **A spoke cluster belongs to exactly one tenant** and is the unit of
  maintenance, versioning and metering.
- **A cell is a label**, grouping a tenant's clusters to attach policy —
  residency, compliance, or anything else a tenant needs applied to a subset. It
  holds no compute and pins no version.

## How maintenance works

The platform's authority ends at a pull request.

Zero-Ops publishes a **bundle**: the complete platform at one tested revision,
named by a tag. When a bundle is published, the platform opens a pull request
against the tenant's own infrastructure repository advancing the clusters that
are due it — the shape of a dependency-update bot. Access is granted by the
tenant through an App installation and is revocable.

Nothing is applied by the platform. A change reaches infrastructure when the
tenant accepts the pull request and the tenant's own control plane reconciles
it. Tenants may configure automatic acceptance.

Support extends as far as the telemetry a box exports. Telemetry is egress-only
and covers control-plane state, never workload data. An upgrade proposal carries
a pre-flight verdict produced inside the box, or is raised as unverified.

## What the platform is, and what the tenant owns

The platform is the set of capabilities Zero-Ops provides and maintains, on
which a tenant's applications execute. The tenant owns the application logic and
the use it makes of those capabilities.

The line is what authored it, not what it does. A database engine, an identity
provider, a message bus, a gateway and a certificate authority are platform:
their behaviour is determined by the platform that ships them. The schemas in
that database, the organisations in that identity provider, the routes through
that gateway and the images behind them are the tenant's.

Required and selectable is a separate question. Every box runs the same cluster
machinery — GitOps engine, composition and cluster lifecycle, secret delivery,
certificate issuance, DNS, gateway, admission policy, capacity lifecycle —
because without it a box cannot reconcile anything. Above that sit capabilities
a tenant selects: a database, an identity provider, messaging, object storage,
metrics.

What decides the side is whether every box needs it, not how central it feels.
Credits are the clearest case. A box sold on to customers needs a way to grant
and spend credits, but a box run as an internal platform -- a team's own
golden path, with no customers of its own -- bills nobody and needs no credits,
billing or metering at all. Making them machinery would run a ledger, and
expose it, in every box to serve a business model only some boxes have, so
they are capabilities: shipped and maintained in every box, run only where a
tenant turns them on.

**Selecting is enabling, not acquiring.** There is no catalog to choose from and
nothing to install. Every capability the bundle provides ships to every tenant,
and a tenant turns on what it runs. A capability left off is still maintained:
it advances with every bundle, and turning it on later needs no action by
Zero-Ops.

## Getting a box

### What a tenant needs first

- **A git organisation.** The platform creates one repository in it, `<tenant>-gitops`, and proposes changes to that repository afterwards. Nothing else in the organisation is touched.
- **A cloud account.** Clusters are created in it, on the tenant's bill. Hetzner today.
- **Two credentials**, both the tenant's and neither held by Zero-Ops:
  - the cloud API token clusters are created with — `HCLOUD_TOKEN` for Hetzner;
  - a token with write access to `<tenant>-gitops`, so Day-0 can commit what it generates there.

### Scaffold

Download the CLI from a release and run it once:

```
# the latest release, for this machine
gh release download --repo kamarkat/kamarkat \
  --pattern "kakt-$(uname -s | tr 'A-Z' 'a-z')-$(uname -m | sed 's/x86_64/amd64/;s/aarch64/arm64/')" \
  --output kakt && chmod +x kakt

./kakt tenant scaffold \
  --tenant acme --box acme-inc --domain acme.example
```

The version a box starts on is the version of the binary that scaffolded it, so
downloading the latest release is how a tenant starts current. `kakt
bundle-version` says which one a binary carries.

Three inputs. The platform version comes from the binary, the control plane is named for the tenant, and the rest is defaulted — `--help` lists what can be overridden.

It creates `acme-inc/acme-gitops`, asks for the two credentials, sets them as secrets on that repository, and starts the bootstrap. Pass `--provider-token` and `--gitops-token` to skip the prompts, or supply neither and the run stops after creating the repository and prints what remains.

Use a released binary. A build from source carries no published version to pin, and refuses rather than scaffolding a repository that cannot bootstrap.

### What runs, and where

Bootstrapping happens in a GitHub Actions workflow **in the tenant's repository**, under the tenant's own secrets. It takes a few hours and can be re-run: its state lives in the repository, so a resumed run skips what already completed.

```
gh run watch --repo acme-inc/acme-gitops
```

Zero-Ops runs nothing during this and holds no credential involved in it.

### What the tenant is left holding

```
acme-gitops
├── registry/clusters/acme/
│   ├── bundle.yaml                 the platform version this cluster runs
│   ├── values.yaml                 what differs from the bundle's defaults
│   └── generated.yaml              applies what Day-0 writes, once it has
├── registry/environments/          a fleet's applications, per environment
├── templates/workload-cluster/     the mould for the next cluster
├── renovate.json                   how upgrade proposals arrive
├── TOKENS.md                       what the template's placeholders mean
└── .github/workflows/              how a cluster is bootstrapped
```

The directory names are kubefirst's (ADR-071): `registry/clusters/<name>` and
`registry/environments/<env>` beside a `templates/` that renders into them. A
reader who knows one should not have to learn the other's names to find the same
file.

Bootstrapping adds `registry/clusters/acme/generated/` and commits it: the facts that
do not exist until Day-0 runs — the Infisical coordinates, the control-plane
address. They belong to the tenant, so they live here rather than with the
platform.

`bundle.yaml` names a chart, a registry and a version. The bundle's content is pulled from the registry rather than copied here, so an upgrade is a change to one field.

## Running a box

### Upgrades arrive as pull requests

When a version is published, a pull request appears against `<tenant>-gitops` changing the pinned version. Merging it moves the cluster; closing it declines the upgrade. Nothing is applied by Zero-Ops.

Each release records what taking it means — the versions it spans, the security fixes among them, whether the predecessor is restorable from it, and the minimum version it may be taken from. Declining is not a breach of support: support follows the version, and one outside its window is a version the platform stops promising maintenance for rather than one it forces.

A tenant that wants the fast path enables auto-merge on that repository.

### Adding a cluster

A workload cluster is a declaration, not a command. `kakt tenant add-cluster` renders `templates/workload-cluster/` into `registry/clusters/<name>/`; commit it. The control plane already running in the box provisions it and keeps it in that state.

There is no second mechanism for this: the thing that keeps a cluster in its desired state is the thing that creates it.

### Choosing what runs

Every capability the bundle provides ships to every tenant. A tenant turns on what it runs by enabling it in `values.yaml`.

A capability left off is still maintained — it advances with every bundle, and turning it on later needs no action by Zero-Ops and no proposal.

## Exit

The software is open source, the control plane is already in the tenant's
account, and the compute is already on the tenant's bill. Ending the
relationship is revoking an App installation. Nothing is withdrawn, because
nothing was held.

## Repository layout

```
cmd/                 kakt, kube-sbt, auth-proxy, mcp-server
internal/            private packages behind those binaries
manifests/           the platform: boundaries, components, compositions, charts
docs/adr/            architecture decision records
docs/runbooks/       operational procedures
scripts/validate/    pre-commit and cluster validators
```

Instances live outside this repository: cluster and cell declarations in a
tenant's infrastructure repository, applications in a tenant's workload
repositories. This repository holds types, never instances.

## Development principles

Contributions must hold to these:

- **Production-ready and idiomatic.** Enterprise-grade, widely adopted
  approaches. Not experimental ones.
- **Strict ADR alignment.** Every solution references the ADRs it rests on.
- **Justified deviations.** Departing from an ADR requires the argument and a
  proposed ADR update, in the change.
- **GitOps first.** Imperative cluster mutation is prohibited. Fix the
  manifest and let it reconcile; never leave a fix as a command.

## Where to start

**To use the platform**, read *Getting a box* above. Nothing in this repository
is needed for that: the CLI carries what it needs, and a tenant's box reconciles
from the tenant's own repository.

**To work on the platform:**

- `docs/adr/` — the architecture, decision by decision. ADR-062 through ADR-072
  define the tenant model, the bundle and its version, promotion, the platform
  boundary, the basis of support, the maintenance promise, how this platform
  differs from kubefirst, and where Day-0 runs.
- `docs/runbooks/bootstrap-and-binaries.md` — the binaries, teardown and state
  management.
- `make build` builds `kakt`. A build from source reports its version as
  `development` and reads platform content from the working tree; a released
  build carries both (ADR-068).

## License

Not yet declared. See `docs/adr/` for the intended model; a licence file is
required before the "open source" claim above is true in any legal sense.
