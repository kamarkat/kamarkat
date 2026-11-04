# PostgreSQL, per application

Every PostgreSQL major on the platform is chosen by the application that owns the
cluster, not platform-wide. The versions differ on purpose, and the parity rule
in `SKILL.md` — one version per component everywhere it runs — does **not** apply
here. This is the exception it names.

## Why it is per application

ADR-112 gives each platform application its own CloudNativePG cluster. An
application's supported PostgreSQL range is its own: one documents 14–18, another
documents only a floor, a third pins a major because its own build does. A single
platform-wide major is therefore either the lowest common denominator for
everyone or unsupported for someone — and whoever picks it becomes an
undocumented authority over software they do not own.

So the question is never "what is the newest PostgreSQL". It is "what is the
newest major **this** application supports", asked once per cluster.

## Two rules that do hold everywhere

**Always pin `imageName`.** CloudNativePG's default image moves with the
operator, so a cluster that omits the field has its major changed by an operator
bump it never asked for. Every cluster names its image.

**Record the constraint, not just the version.** A pin without its reason reads
as stale to the next person, who bumps it. The catalog's `behind` item is where
the reason lives; `held` or `declined` with the consumer's own limitation named.

## Reading the current state

```bash
for f in manifests/hub-core-services/*/database/*-db.yaml \
         manifests/hub-core-services/*/*/database/*-db.yaml \
         manifests/spoke/spoke-catalog/providers/*/cnpg-cluster.yaml; do
  printf "%-60s %s\n" "${f#manifests/}" "$(grep -oE 'imageName: .*' "$f")"
done
```

What to expect from it:

- **Hub application clusters** — one per application under
  `hub-core-services/<app>/database/<app>-db.yaml`. Majors differ.
- **The billing cluster** is the exception to the exception: it runs a
  platform-built image (`postgres-partman`), because the engine's schema creates
  `pg_partman` on an empty database and a cluster cannot load an extension its
  image does not ship. Its major is set by `images/postgres-partman/Dockerfile`,
  not by an `imageName` tag, and it is pinned there by digest. Upgrading it is a
  CI image build, not a pin edit.
- **Spoke clusters** are the shared store tenant applications draw from through
  `TenantDatabase`. That composite exposes no version field, so the major is a
  platform-owned contract — see below.

## What constrains a choice

Ask these per cluster, and write the answer into the catalog item:

1. **Does the application document the major?** A documented range is the floor
   and ceiling. An undocumented ceiling is not permission.
2. **Is there a known defect at the newest major?** A documented range can still
   carry an open initialisation failure, which matters most on a *fresh* install
   — which is what creating an environment is.
3. **Does the application run migrations on start?** If so the move needs its own
   validation, not just a passing render.
4. **Does anything else have to match?** The partman image's base major and its
   `postgresql-NN-partman` package move together; a mismatch fails the build.

## TenantDatabase: the major is platform-owned

`TenantDatabase` takes a tenant, an app and a scope — **no version field, and no
cluster field.** Its composition creates `Database`, `Role` and `Grant` inside
the shared spoke cluster; it does not provision a cluster. So the major is a
property of `spoke-catalog/providers/*/cnpg-cluster.yaml`, which the platform
owns and reconciles.

That is the right owner, and exposing the major on the composite would be wrong
in both directions: either one cluster per major, multiplying stateful
infrastructure per tenant against ADR-014, or a field that silently does nothing.

The consequence to be deliberate about: the spoke's major binds every future
tenant application on that spoke, and they cannot opt out. Choose it as a
platform commitment, not as "the newest that rendered".

## Major upgrades are not pin edits

CloudNativePG does not upgrade a major in place by changing `imageName`. On a
cluster with data a major move is a migration with its own plan. On a newly
created environment there is no data, so the choice is free — which is why the
right time to set these is at creation, and why a fresh environment should not
inherit an older environment's majors by default.
