# <TENANT_ID>-gitops

Infrastructure for the **<TENANT_ID>** box. This repository is yours: it lives in
your organisation, your control plane reconciles it, and your cloud account holds
everything it describes.

## Layout

    registry/clusters/<name>/bundle.yaml     the platform version this cluster runs
    registry/clusters/<name>/values.yaml     your configuration for it
    environments/<env>/<app>/values.yaml     what this application is: its cluster, quotas, hostnames
    environments/<env>/<app>/<workload>/     the version of one workload it runs
    templates/workload-cluster/              render this to add a cluster
    templates/app/                           render this to add an application
    templates/workload/                      render this to run one of its workloads

## Who writes what

The platform proposes changes to `bundle.yaml` as pull requests, in the manner of
a dependency-update bot. It never writes `values.yaml`, and it cannot merge its
own proposal.

You write `values.yaml`. It holds only what differs from the bundle's defaults,
so a default can change in a new version without touching anything here, and an
override you have made survives across versions.

The two are separate files so the two writers never conflict. Merging a proposal
is a normal review; accepting them automatically is a repository setting and
changes nothing about what the platform may do.

## Adding a cluster

Copy `templates/workload-cluster/` to `registry/clusters/<name>/`, replace the tokens, commit.
Your control plane provisions it.

## Running an application

The application first: copy `templates/app/values.yaml` to
`environments/<env>/<app>/values.yaml` and fill in its id, the cluster it runs on
and its quota. That file existing is what makes the application real -- your
control plane globs for it -- and it gets the application a namespace, a quota
and its hostnames.

Then copy `templates/workload/Chart.yaml` to
`environments/<env>/<app>/<workload>/Chart.yaml`, one directory per workload,
each with one dependency.

Your application's own repository holds its source and its Helm chart, builds it,
publishes that chart to a registry, and then commits the new version into the
`Chart.yaml` here. That commit is the deployment: your control plane reconciles
it on the next pass.

What lives here is a version and your values -- never an image digest, an overlay
or a rendered manifest. Two consequences are worth knowing. What you are running
is legible from this file without resolving anything, and a rollback is an edit to
one line, because the version you are going back to still exists in the registry.

Your build writes `Chart.yaml`; you write `values.yaml`. Same division as
`bundle.yaml` and `values.yaml` above, and for the same reason: two writers, no
shared field, nothing to arbitrate.

## Supplying an application's secrets

Three kinds of value reach a workload, and they are separated by **who owns the
value** rather than by how they are delivered.

**The platform generates what it owns.** A confidential OAuth client, a cache, a
database: you declare the CAPABILITY in your application's `values.yaml` and the
credentials are created and delivered for you. You never see them, and listing
their keys yourself claims to supply something you cannot produce.

**Git carries what is not secret.** Model names, bucket names, base URLs,
endpoint identifiers go in `config`. They are reviewed in the change that alters
them and readable without a credential. A model name should be a one-line commit,
not a credential rotation.

**You supply the rest.** Declare each one under `secrets` with the capability it
serves and the workloads that read it. Then, from the root of this repository:

```
kakt app secrets status <env> <app>       what is declared, and what is present
kakt app secrets set <env> <app> <KEY>    supply one value
```

`set` prompts with echo disabled, or takes `--stdin` so a password manager can
pipe into it. There is deliberately no flag that takes the value as an argument:
that puts it in your shell history and in the process table. There is no batch
form either, because a batch means a file and a file means secrets sitting on
your machine.

Rotation is `set` again. The value is replaced, the delivery mechanism sees the
change on its next refresh, and the workload restarts on it.

`status` prints key NAMES and never values, and is safe to paste into a ticket.
It also tells you what an absent secret actually costs: secrets are delivered one
object per workload, and that object is ATOMIC -- one key the provider cannot
supply fails all of them, so every container reading any key in it cannot start.
That is why the declaration names workloads, and why a non-secret does not belong
in `secrets`: a mistyped bucket name there withholds a credential from something
unrelated.

Supplying them is three commands, and the CLI is the only way to do it:

```
kakt app secrets template <env> <app>                                       the keys this app declares, empty
kakt app secrets import <env> <app> --from-env <file>                        shows what would be written
kakt app secrets import <env> <app> --from-env <file> --confirm              writes it
```

Each app commits `environments/<env>/<app>/.env.example`: exactly the keys it
declares, empty, with the capability each serves as its comment, generated by
`kakt app secrets template <env> <app>`. Copy it to `.env` beside it
(gitignored), fill in the values, then import -- without `--confirm` the import
shows what it would write and writes nothing. An example whose keys no longer
match the declaration is regenerated by the same `template` command.

An entry you have not filled in is never written. It would otherwise replace a
secret that is already supplied with nothing, and the workload reading it would
start and fail -- so a half-filled file writes the half you filled and reports
the rest as empty.

For one secret, or to rotate one, `kakt app secrets set <env> <app> <KEY>`
prompts with echo disabled and touches no file at all. `kakt app secrets status
<env> <app>` says which of the declared keys are supplied.

**This repository ships no scripts.** There is one way to supply a secret and it
is the CLI: a script in your repository would be a second answer to "which keys
does this application declare", pinned to the version of the platform that
rendered it, and drifting from the declaration the moment either changed.

You never type the tenant or the cluster. Both are read from
`environments/<env>/<app>/values.yaml` -- `tenantId` and `clusterName` -- and the
storage path is derived from them, so the place a value is written and the place
the workload reads it from cannot disagree. Naming that path by hand is how a
secret ends up under a cluster the application does not run on, which surfaces
later as a store that appears broken rather than as the typo it was.

### How often you do this

Once per secret, for the life of the box -- not per application, and not per
rebuild.

A key is supplied once and delivered to every workload that declares it, so a
application's count is its number of distinct secrets and not its number of
applications: one shared token read by four workloads is one `set`.

**Rebuilding the cluster does not mean re-entering them.** Your secret store's
data is in the platform database, which backs up to object storage outside the
box, and the master keys that decrypt it are escrowed to an Infisical you control
and the platform does not. A rebuild restores the database and reads the same
master keys back from the escrow, so every secret is present and readable before
any workload starts. That is what the escrow is for: an escrow only one side can
reach is a backup with no restore.

So the realistic lifetime of an application with ten secrets is ten `set` calls -- or one
`import` if you are migrating -- then nothing until you rotate one or declare a
new capability. If you find yourself re-entering values after a rebuild, the
escrow or the database backup is broken, and that is the thing to fix rather than
a step to repeat.

### Migrating an application that predates this

If your values are currently in a `KEY=VALUE` file:

```
kakt app secrets import <env> --from-env <file>
```

It lists what it would write and writes nothing until you add `--confirm`, and it
writes **only keys your application declares** -- a seed file usually carries
configuration and connection settings alongside the secrets, and those belong in
`config` or nowhere. Delete the file afterwards: what remains is a second copy
that nothing rotates.

## Leaving

Revoke the platform's app installation. Proposals stop arriving; nothing stops
running. Everything here is already yours.
