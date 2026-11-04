# ADR-076: Reaching a Box You Own

**Date:** 2026-09-11

**Status:** Proposed

## Context

ADR-065 gives each tenant a control plane in their own cloud account and says the
platform holds no credential of theirs. ADR-062 adds that exit is not a migration:
a tenant who stops buying maintenance keeps a running control plane.

Neither says how anyone reaches it. Five facts bear on that.

**Day-0 produces an admin credential and then discards it.** CAPI writes the
cluster's admin kubeconfig into a Secret, and the CLI copies it to
`k8-secrets/kubeconfig/<cluster>.kubeconfig`. That directory is gitignored, the
bootstrap workflow never mentions it, and it is not a registered ADR-045 artifact.
Under ADR-072 Day-0 runs on an ephemeral runner, so the file is destroyed with the
job. A tenant finishes onboarding owning a cluster they hold no credential for.

**The platform's own operation hid it.** Every bootstrap so far has been the
platform's, on a workstation where `k8-secrets/` persists. Nothing exercised "the
job ended, now what", so a tenant-facing gap looked like a working mechanism.

**Documentation already assumes access nobody has.** The restore runbook opens with
`kubectl -n platform-data get cluster platform-db` -- a command the tenant this
platform is built for cannot run.

**The box's own secret store cannot hold the answer.** Infisical runs inside the
cluster. A credential needed when the cluster is unreachable cannot live in it, and
proposing otherwise is circular.

**An escrow already exists, in the wrong place.** `hub-operator` copies the
Infisical master keys out of the box and restores them when the in-cluster copy is
gone. It is out-of-band, keyed by cluster, and it works -- but it wrote to AWS
Secrets Manager under one account and one prefix, which is the platform's while
every cluster is the platform's.

**Infisical is already the box's secret store, and Infisical has a hosted form.**
The client is the same code against a different URL: Infisical's own source connects
to other instances with `POST {instanceUrl}/api/v1/auth/universal-auth/login` and a
machine identity, and the platform's client already reads its base URL from the
environment. A hosted Infisical the tenant controls is therefore out-of-band without
being a second vendor, a second SDK, or a second thing to learn.

**The comparable system mints rather than stores.** kubefirst's API exposes
`POST /kubeconfig/:cloud_provider`, and the handler fetches from the cloud provider
using the caller's own cloud credential, which the request carries. Nothing is
retained. It also filters `terraform/base/kubeconfig` out of git commits, having
shipped one there once. That pattern is unavailable here: it depends on the cloud
provider having a "give me the kubeconfig" endpoint, and a CAPI cluster on plain
Hetzner servers has none -- the only copy lives inside the cluster.

## Decision

**A tenant reaches their box as a person, through their own identity provider. The
admin credential is a break-glass artefact, escrowed outside the box, in an account
the tenant controls.**

### Day-to-day access is OIDC, not a kubeconfig

The box runs an identity provider, and the ClusterClass carries the means to point
the API server at it -- `oidcIssuerURL` and `oidcClientID`, applied together or not
at all. Access is therefore a person authenticating as themselves, with the
authorisation their role carries, revocable by removing them.

This is what makes ownership real rather than stated. A shared admin credential is
not owned by anyone: it cannot be attributed, it cannot be revoked for one person,
and the act of giving someone access is the act of copying it.

The capability is not the configuration. Both values default to empty and the patch
is conditioned on both being set, so a box is delivered with the mechanism present
and unset, and until it is set the API server trusts no issuer. That is deliberate
-- the issuer runs on the cluster and does not exist when the control plane is
created -- but it means the admin credential below is the only way in until the
values are supplied, which is the state every box built so far is in.

### A generated kubeconfig authenticates through an approved credential plugin

OIDC access is delivered by a kubeconfig that obtains credentials dynamically
rather than carrying a static token, so the question of what it is permitted to
execute arises the moment the platform generates one.

Any generated kubeconfig that uses exec-based authentication names an explicitly
approved credential plugin. An arbitrary executable path is not permitted.

The reason is upstream, not local. A kubeconfig's exec configuration names a
program to run and is trusted by whoever holds the file, so a kubeconfig obtained
from somewhere else is a request to execute something; Kubernetes has moved
toward constraining which plugins may be invoked, and the exact mechanism is
version-dependent. This decision records the invariant rather than the mechanism,
so it holds across versions that implement the constraint differently and across
those that do not implement it at all.

Nothing today generates an exec-based kubeconfig: Day-0 writes the admin
credential, which is a static kubeconfig and break-glass. The constraint applies
when kubeconfig generation is built, and is recorded now because that is when it
is cheap to honour.

### Enabling OIDC is a desired-state change, not an act performed on a cluster

The values belong to the cluster topology recorded in the tenant's repository, and
that record is authoritative: it is reconciled continuously and self-healed, so a
change made directly against the API server's configuration is reverted to what the
repository says. Supplying them is therefore the same class of change as altering
worker capacity -- an edit to the declared topology, reconciled like any other.

This follows from ADR-039 rather than adding to it. The topology has one System of
Record; a second path that writes the same fields would be a second source for one
question, and the reconciler would resolve the disagreement by discarding whichever
was not in the repository.

It is not part of Day-0. Beyond the ordering problem -- the issuer is not running
when the control plane is created -- the API server's authentication configuration
is carried by the control plane machines, so supplying these values replaces them.
A box has one control-plane node, so the change costs a control-plane rollout and
the API server returns on a different endpoint. ADR-040 puts continuous change in
Day-1+, and this is a Day-1 change to a Day-0 artefact's declared state.

### Identity-provider administration is a separate plane from cluster configuration

Two credentials are involved in human access and they are deliberately not the same
credential, nor held in the same place.

The first is the human's, minted per login by the identity provider and carrying
only the authorisation their role grants. The second administers the identity
provider itself: registering the client that the API server names is an operation
against the identity provider's management plane, and that credential can rewrite
how the box authenticates.

The hub holds neither permanently. Once the client exists, what the API server needs
is an issuer URL and a client identifier -- both public, both belonging in the
repository with the rest of the topology. Nothing privileged has to live on the box
for a person to log in.

Keeping the management credential off the box is the point. Storing it there to
spare a separate administrative step would place the identity provider's
administration inside the system it authenticates, and give every box a standing
credential capable of altering its own login. ADR-003 already forbids a Kubernetes
Secret being the System of Record for secret material; this says something stronger
about this particular credential, which is that the box has no reason to hold it at
all.

Where this is automated, it is automated against the identity provider's management
interface. The initial-instance configuration is applied once, when an instance is
first created, and does not reach an instance that already exists -- so building on
it would leave existing and new boxes on different paths to the same outcome, and
would route the most privileged credential on the box through a setup path whose
default destination is the job's own output.

### The credential that registers the client is borrowed, never held

The client the API server names has to be registered in the identity provider,
and that is an operation against its management plane. The box does not hold a
credential for it.

A dedicated service account is created for the registration, its token carries an
explicit expiry, and it is revoked as soon as the client exists -- on every path,
including a failed registration and an abandoned run, because those are the paths
where a credential is most likely to be left behind. Revocation failing is
reported as a failure of the whole operation even when the client was created: a
box that kept a live management token has not reached the state this describes,
whatever else succeeded.

The token's identifiers are as mandatory as the token. A credential that cannot
be revoked is refused before it is used rather than discovered afterwards.

The current user and application services are used rather than the deprecated
management endpoints, and that is not only currency: on the current service an
expiration date is a required field, so a token without one cannot be created by
accident. The property this decision depends on is enforced by the interface
instead of remembered by whoever calls it.

Nothing about this is a secret-management problem, so no secret store is
introduced for it. Routing a single-use bootstrap token through the box's own
secret store would give it a durable home, a lifecycle, and a second system that
must be reasoned about -- for a credential whose entire purpose is to stop
existing. It is received as an input, used, and destroyed.

What survives is public: a client identifier, recorded with the topology. That is
the reason the box needs no management credential at runtime.

### The admin credential is escrowed, not distributed

The cluster's admin kubeconfig is what remains when OIDC cannot be used -- the
identity provider is down, the cluster is broken, or nobody is left who can log in.
It is escrowed beside the Infisical master keys, under the same
`/hub-operator/<clusterID>` path.

Escrow rather than distribution: one copy, in one place, reachable by the tenant and
not held by anyone as a matter of course.

### The escrow is an Infisical the tenant controls and the box does not host

Infisical Cloud, or an Infisical the tenant runs elsewhere. Not AWS, and not the
box's own.

Not the box's own, because the master keys are what decrypt it: an escrow inside the
thing it protects is not an escrow, and it is unreachable at exactly the moment it is
needed. The implementation refuses an escrow URL equal to the in-cluster one rather
than accepting a configuration that cannot work.

Infisical rather than AWS because the box already runs Infisical, the API is the
same one, and the client already takes its URL from the environment. Requiring AWS
would add a second cloud account, a second credential type and a second SDK to a
platform that otherwise needs one cloud -- and ADR-070 measures the floor in exactly
those terms.

### The recovery contract

Normative. Each artefact below is held under `/hub-operator/<clusterID>`, and each row
states its SCOPE, who holds AUTHORITY over its value, and whether recovery of the box
requires it.

| Artefact | Scope | Authority | Recovery | Normal use |
|---|---|---|---|---|
| `secret-encryption-key` | per cluster | the escrow, by the provisioning contract (ADR-003 §6, ADR-100) | **required** | read from the node's own file |
| `secret-encryption-key-previous` | per cluster, **only during a rotation** | the escrow, for the length of the rotation | **required while present** | read from the node's own file as the second provider |
| `admin-kubeconfig` | per cluster | the platform's cluster lifecycle | **required** | none — break-glass only |
| `zitadel-masterkey` | hub | Infisical; the escrow holds a recovery copy | **required** | read from Infisical by the workload |
| `infisical-master-keys` | hub | Infisical's own bootstrap; the escrow holds the only recoverable copy | **required** | read at the secret store's start |
| ordinary application secrets | as ADR-003 declares | **Infisical** | per that system's own contract | delivered by ESO |

**Authority is declared per artefact, never inferred.** The escrow is the recovery
authority ONLY for an artefact whose row says so. It does not override ADR-003's rule
that Infisical is the System of Record for secret material: for `zitadel-masterkey`
the authoritative value is Infisical's and the escrow holds a copy, while for
#### Cloud KMS keys are not escrow artefacts

**A Cloud KMS encryption key is never an escrow artefact, and exporting or duplicating
one into the escrow is prohibited.** ADR-100 moves each cluster's etcd
key-encryption key to Google Cloud KMS, where it is non-exportable by construction.

That is not an oversight in the table below — it is the point. A copy of that key
outside Cloud KMS would be an offline decryption path for every etcd backup taken
while it was in force, which is precisely the exposure moving it out of the box
removes. Recovery of an encrypted cluster therefore depends on the **continuity of the
external KMS authority**, not on anything this escrow holds.

The escrow's scope is unchanged: it holds secret VALUES that cannot be regenerated —
the Zitadel masterkey, Infisical's master keys, admin kubeconfigs. Those have a
different custody model from a cryptographic key, and conflating the two would mean
either a second secret store or a key copied into the first.

`secret-encryption-key` and `secret-encryption-key-previous` below are the
**`secretbox`** keys, which do live here because that provider keeps its key on the
node and the escrow is the only copy. They become irrelevant for any cluster that
moves to Cloud KMS, and are removed from that cluster's contract when it does.

#### The one conditional row, and why it is not a third state

Every other artefact here is present or the box is broken. `secret-encryption-key-previous`
is present only between staging a rotation and verifying it, and absent otherwise —
which looks like the "usually present" hedge the no-third-state rule above forbids.

It is not, because its presence is not a matter of luck or history: **it means a
rotation is in flight**, and its absence means one is not. During that window etcd
holds objects sealed under BOTH generations, so recovering the cluster needs both keys;
once every Secret has been rewritten and verified under the new key, the old one
recovers nothing and is no longer part of the contract.

So the row reads "required while present" rather than "required" or "optional". An
operator finding it in the escrow has learned something true about the cluster: someone
started a rotation and has not finished it. `kakt encryption rotate` writes it, and
`--finalize` is the step after which it stops mattering.

`secret-encryption-key` there is no Infisical copy at all and the escrow IS the
source. Reading "the escrow is authoritative" as a general principle would invert
ADR-003 for every ordinary secret, which is why this is a table and not a sentence.

**Every cluster has an independent administrative recovery path.** `admin-kubeconfig`
is scoped per cluster and required even where normal operations are mediated by the
management cluster, because a catastrophic failure of that cluster or of the
automation must not leave a cluster with no way in. It is not for routine access —
day-to-day entry is each person's own identity through the identity provider — and
escrowing it does not make a workload cluster routinely operator-accessible.

There is no third state. An artefact is either required and escrowed, or explicitly
not part of the recovery design with a tested alternative named here. "It is usually
present on the node" is not a recovery contract: a node is what a recovery replaces.

#### Why `zitadel-masterkey` is required, mechanically

Stated because the consequence is larger than the name suggests, and was proven
against the deployed release rather than assumed.

The masterkey is a KEY-ENCRYPTION key. The identity provider stores its data
encryption keys in its own database, encrypted with it, and decrypts them at start —
`system.encryption_keys`, AES, failing with "unable to decrypt key". Those data keys
are what protect OAuth client secrets, identity-provider configuration, signing keys
and one-time-password material.

So a box restored without it does not lose some columns. It recovers the database,
recovers the secret store, and still holds no key for any of that material — because
the one value that unlocks every data key was the one thing not kept. That is why it
is required rather than advisable, and why restoring Infisical is not sufficient on
its own.

**The test for membership is whether the value can be regenerated without loss.**
A credential that can be rotated — a database password, a session cookie key — is
not escrowed: losing it costs a rotation. A key that something was encrypted WITH is
escrowed, because regenerating it destroys what it protected.

Applied to the values the platform generates for a box, that test selects the three
above and rejects the rest: the control-plane, centralised and identity database
passwords are rotatable, and so is the API gateway's session cookie key.

**This list is part of the decision and MUST be updated when the set changes.** It
is the only enumeration of what a box can be rebuilt from; a value that belongs here
and is absent is invisible until the day the cluster is gone, which is the same
failure the escrow being mandatory exists to prevent. A preflight asserts the list
and the implementation agree, so the omission is caught at build time rather than
discovered during a recovery.

**`zitadel-masterkey` was listed before it was implemented** (2026-10-02), and the
preflight reported the difference until it closed. That ordering is the practice this
list is meant to make possible: the list states what the escrow must hold, the
difference between that and what it holds is work, and the gap is visible while the
work is outstanding rather than after a recovery has failed.

A box provisioned before that date generated its key with no escrow. The first
reconcile after the change finds the key present in the box's own secret store, so
nothing generates and nothing escrows — **the existing value must be escrowed once,
by hand, and a box is not recoverable until it has been.**

### The escrow account is the tenant's

The credentials reaching it are supplied with the tenant's others at scaffold time
and stored on their repository. The platform never holds them.

This is not a preference. The escrow holds the key to a box's secret store and the
key to its API server -- the two credentials that, together, are the box. ADR-065
says the platform holds no cloud, cluster or secret credential belonging to a
tenant, and an escrow in the platform's account would hold both. In the tenant's
account the platform holds neither, and the tenant can revoke it without asking.

The existing mechanism moves with it: the same client, the same `clusterID`-scoped
path, a different account.

### The escrow is required

A box is not built without one. Scaffolding refuses to complete, and a bootstrap
refuses to start, when the escrow credentials are absent.

Optional was considered and is refused for one reason: the cost of not having an
escrow is paid entirely in the future, by someone who did not make the choice. A
box without one bootstraps, runs, and behaves identically for months. The
difference appears on the day the cluster is gone -- and on that day the escrow
cannot be added, the Infisical master keys are unrecoverable, and every secret the
box held is lost with it. A default whose consequence is invisible until it is
irreversible is not a choice a tenant is making; it is one being made for them.

The platform is sold as maintenance for organisations who cannot staff a platform
team (ADR-069). Handing such a tenant a box that is one incident away from total
loss, and recording that they opted into it, is not a service.

This raises the floor, and the choice of Infisical over a second cloud vendor is
what keeps the rise small: an account on the product the box already runs, free at
the size these boxes start at.

### Nothing is committed to git, encrypted or not

An encrypted kubeconfig in the tenant's repository was considered and is rejected
below. The short reason is that git history is permanent: ciphertext committed today
is decryptable by whoever holds the key in five years, and a credential with a
one-year life leaves four years of exposure for no benefit.

## Alternatives considered

**Escrow to AWS Secrets Manager.** Rejected, having been built. The mechanism
worked and the cost was structural: every tenant would need an AWS account for a
platform that otherwise needs one cloud, and a required second vendor raises
ADR-070's floor for everyone to serve a case Infisical already covers. The IAM
policy was also account-wide and prefix-scoped, so using it as-is would have put
tenants' keys in the platform's account -- and moving it to theirs solved custody
while leaving the second-vendor cost in place.

**SOPS-encrypted kubeconfig in the tenant's repository.** Rejected, though it is the
closest workable alternative and solves the out-of-band problem the same way. Three
costs decide it. Git history is permanent, so the ciphertext outlives the credential
by years and the encryption key becomes the only thing standing between a repository
read and cluster admin. It introduces a second secret mechanism alongside the
Infisical and ESO path the box already runs, with its own key distribution. And the
key itself must live somewhere the cluster is not, which is the question the proposal
was meant to answer. An escrow in a secret store the tenant already authenticates to
answers it once.

**Escrow in an account the platform controls.** Rejected whatever the vendor. It is
the smallest change -- the mechanism exists and works -- and it makes the platform
custodian of the two credentials that constitute a tenant's box. ADR-065 exists to
prevent exactly that, and the convenience is entirely the platform's.

**An optional escrow, defaulting to off.** Rejected, having been written. It keeps
the floor where ADR-070 puts it and keeps the platform usable by a tenant with one
cloud account -- both real. It fails on when the cost lands: a box without an escrow
is indistinguishable from one with it until the cluster is lost, at which point the
choice cannot be revisited and the box cannot be rebuilt. The tenants this platform
is for are the least equipped to evaluate that trade at scaffold time and the most
harmed by getting it wrong.

**An optional escrow that warns loudly.** Rejected for the same reason, one step
later. A warning at the moment of creation is read by someone who has not yet
operated the box, and is not read again. The platform would have said the true
thing and still shipped the unrecoverable box.

**Mint on demand from the cloud provider, as kubefirst does.** Not available. It
depends on the provider exposing the cluster's kubeconfig, which is true of managed
Kubernetes and false of a CAPI cluster on plain servers: the only copy is the Secret
inside the cluster, so fetching it requires the access it would grant. Should a
managed-Kubernetes provider be supported, this becomes the better answer for that
provider and this decision is revisited for it.

**Distribute the admin kubeconfig to the tenant at handover.** Rejected. It is what
the platform accidentally does today for its own box, and it is the arrangement
every other decision here exists to avoid: a credential that cannot be attributed,
cannot be selectively revoked, and is copied by the act of granting access.

## Ownership

| Resource Class | System of Record | Lifecycle Owner | Reconciler | Consumer | Phase |
|---|---|---|---|---|---|
| Human access to a box | tenant's identity provider | Tenant | — | People | Day-1+ |
| API server OIDC capability | `zero-ops` ClusterClass | Platform | CAPI | kube-apiserver | Day-0 |
| API server OIDC configuration | the cluster topology in the `<tenant>-gitops` repository | Tenant | ArgoCD, then CAPI | kube-apiserver | Day-1+ |
| Client registration for cluster access | tenant's identity provider | Tenant | — | kube-apiserver, People | Day-1+ |
| Client registration credential | tenant's identity provider | Tenant | — | registration only, then revoked | Day-1+ |
| Admin kubeconfig | the cluster's CAPI Secret | Tenant | CAPI | break-glass only | Day-0 |
| Escrowed copy | tenant's Infisical (Cloud or self-run) | Tenant | hub-operator | break-glass only | Day-1+ |
| Escrow credentials | `<tenant>-gitops` repository secrets | Tenant | — | hub-operator | Day-0 |

The platform appears in no row but the one describing what it builds. The capability
and the configuration are separate rows on purpose: the platform ships the means to
trust an issuer, and which issuer a box trusts is the tenant's, recorded in their
repository. The registration credential has a row whose consumer is an
operation rather than a component, which is the ownership statement -- it exists
for the length of one registration and nothing runs on it afterwards.

See ADR-039 for the complete ownership matrix.

## Consequences

### Positive

A tenant can reach the box they own, which ADR-062 and ADR-065 both assume and
neither provided. The independence those decisions claim becomes testable.

Access is attributable and individually revocable, so granting it to a colleague is
not the same act as copying a credential.

The escrow holds what a box cannot be rebuilt without, in an account the platform
cannot read. Losing the cluster stops being the same as losing its secrets.

One escrow mechanism serves both the Infisical master keys and the admin kubeconfig,
rather than a second one appearing for the second secret.

Which issuer a box trusts is declared in the tenant's repository and reconciled,
so it is reviewable, revertible, and has one source. Enabling human access is not
an operation performed against a running cluster and therefore does not depend on
holding the credential it exists to stop needing.

No box holds a credential capable of altering how it authenticates. What the API
server needs is public, which is why it can live in a repository at all, and the
credential that registers the client is revoked as part of the operation that
uses it rather than by a later step someone has to remember.

### Negative

A tenant needs an Infisical account outside their box. It is free at the size these
boxes start at and it is the same product the box already runs, but it is still a
second account and a second place to lose access to.

The escrow is a third party in the recovery path. A tenant who cannot reach
Infisical Cloud cannot reach their break-glass credential, and ADR-063's custody
argument is precisely about not depending on services that can be withdrawn. An
Infisical the tenant self-hosts elsewhere answers this and costs them an instance to
run; both are permitted and the choice is theirs.

The escrowed kubeconfig expires. Nothing configures certificate rotation, so
kubeadm's one-year client certificate applies, and an escrowed copy silently stops
working. Detecting that is not yet built, and a break-glass credential discovered to
be expired during an incident is worse than one known to be absent.

An escrow nobody has restored from is a backup nobody has tested. The same is true
today of the Infisical master keys, and adding a second artefact to an untested path
does not make it tested.

OIDC access depends on the box's own identity provider, so losing it costs both the
normal path and one of the two reasons the break-glass path exists. The escrow
covers it, which is why the escrow is not optional in practice for anyone who cares
about recovery.

Supplying the OIDC values replaces the control-plane machines, and a box has one
control-plane node, so enabling human access costs an interruption and a changed API
server endpoint. That cost is why it is not done during Day-0 and not done
implicitly.

Registering the client that the API server names remains a separate operation
against the identity provider, so a box is not reachable by a person until both
halves are done. Delivering the mechanism unset is what makes the gap possible, and
a box in that state looks configured -- the variables exist, the binding exists --
while the API server trusts nothing.

### Positive and negative at once

Making the escrow optional keeps the platform usable by a tenant with one cloud, and
guarantees some boxes will be unrecoverable. Both follow from the same choice, and
the second is stated at scaffold time rather than discovered.

## Impact

- **Amends ADR-062.** Onboarding produces a box the tenant can reach. The escrow
  credentials join the secrets scaffolding asks for and stores on the tenant's
  repository.
- **Confirms ADR-065.** The platform holds no cloud, cluster or secret credential
  belonging to a tenant, including the escrow. Exit remains not a migration: a
  tenant keeps a running control plane and, now, the means to enter it.
- **Amends ADR-045.** The admin kubeconfig is a bootstrap-generated artefact whose
  destination is an escrow rather than the repository, because it is a credential
  and the registry's artefacts are configuration.
- **Constrains ADR-070.** The escrow is required, so it is part of the floor. It is
  an Infisical account rather than a second cloud vendor precisely so that floor
  moves as little as possible: free at the size these boxes start at, and the same
  product the box already runs.
- **Amends ADR-040.** The API server's OIDC configuration is a Day-1+ change to state
  a Day-0 artefact declares. Day-0 creates the cluster and the topology record; the
  values are supplied afterwards, because the issuer they name does not exist while
  the control plane is being created.
- **Confirms ADR-039.** The cluster topology has one System of Record, and the OIDC
  values are part of it. No second path writes them.
- No change to ADR-063 or ADR-064: nothing here travels with the bundle or moves
  with a version.
- **Amends ADR-060 and ADR-003 (2026-10-02).** The identity provider's encryption
  key for data at rest joins the escrow. It is generated once and never rotated,
  so it meets this ADR's membership test, and until it is escrowed a box's identity
  data cannot be recovered even when its secret store can. The live source is
  unchanged: the box's own secret store serves it, and the escrow holds a recovery
  copy, because a key read at startup must not depend on reaching another site.

## References

- ADR-003: Secret Lifecycle
- ADR-039: Platform Ownership Model
- ADR-040: Day-0 vs Day-1 Lifecycle Boundary
- ADR-045: Bootstrap-Generated GitOps Artifacts
- ADR-062: Onboarding and Scaffolding a Tenant
- ADR-065: The Control Plane Ships Into the Box
- ADR-070: The Minimum Supported Box
- ADR-071: How This Platform Differs from kubefirst
- ADR-072: Tenant-Controlled Day-0 and Declarative Cluster Lifecycle
