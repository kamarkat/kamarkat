# `kakt init-secrets`

What this command creates, in order, and why each step is where it is.

These are the Secret Zero credentials: the ones that cannot come from Infisical,
because they are what lets Infisical start. Everything else is delivered by
ExternalSecrets once Infisical is up, and nothing here is re-created when it
already exists — an existing bootstrap credential is treated as immutable, since
overwriting one re-keys a database or an encrypted store out from under itself.

## Step 1 — Infisical's own cryptographic material

**`GenerateInfisicalCryptoSecrets`**, in `platform-security`:

- **`infisical-secrets`** — `ENCRYPTION_KEY` and `AUTH_SECRET` (32 hex characters
  each, for AES-256-GCM and JWT signing), `REDIS_URL` carrying a generated Redis
  password, and `DB_ROOT_CERT`.
- **`infisical-redis-credentials`** — the same Redis password, for Redis itself.

`DB_ROOT_CERT` is deferred: it is the CA CloudNativePG generates for the cluster,
which does not exist until the cluster does. `UpdateInfisicalSecretsWithCNPGCert`
fills it in once `infisical-db` is Ready.

## Step 2 — the database credential, and the connection that uses it

**`InstallPostgresConnectionSecret`**:

- **`infisical-db-credentials`** (`platform-data`, `kubernetes.io/basic-auth`) —
  `username: infisical` and a 32-character password. This is the Secret
  CloudNativePG's `bootstrap.initdb.secret` reads on `infisical-db`, so the owner
  role and the database are created from it (ADR-023 addendum). It is written
  BEFORE boundary 02 creates the cluster, because `initdb` runs once and cannot be
  repaired afterwards.
- **`infisical-postgres-connection`** (`platform-security`) — the connection
  Infisical's pods mount: `DB_HOST` is the cluster's read-write service, not a
  pooler, because `infisical-db` has none (ADR-112 §2), and `DB_SSL_MODE=require`.

The two live in different namespaces deliberately: a Secret belongs where it is
consumed, and the credential CNPG reads is not the one Infisical's pods read.

## Step 3 — wait for Infisical

**`WaitForInfisicalHealth`** waits for `infisical-db` to be Ready and then for
Infisical's own workload. The database half closes a real boot race: Infisical
crash-loops if it starts before its database service exists, and the wait names the
cluster through one constant so the gate and the connection cannot point at
different clusters.

## Step 4 — authenticate to Infisical

**`InstallInfisicalAuthFromInfisical`** resolves the Machine Identity credentials
(`infisical-auth`) this box uses for every later call.

## Step 5 — hand the bootstrap credential to Infisical

**`InstallPlatformDatabaseCredentials`** uploads `infisical-db-credentials`'
username and password into Infisical, which makes Infisical the system of record
for the credential that bootstrapped it. From here ExternalSecrets delivers every
other credential, including the application role credentials the hub-operator waits
for.

## Step 6 — restart what holds stale credentials

**`RestartPlatformWorkloads`**, only when something above changed: `redis-master`
in `platform-data`, and `infisical-standalone-infisical` in `platform-security`
(StatefulSet or Deployment, whichever the chart made).
