# Atlas Integration Analysis

**Spec**: spoke-pool-provisioner  
**Component**: Atlas CLI + Atlas Kubernetes Operator  
**Purpose**: GitOps-driven tenant schema provisioning with automatic drift detection and reconciliation  
**Created**: 2026-04-08

---

## 1. Integration Context

### 1.1 Position in Spoke Pool Provisioning Flow

```
CNPG Cluster Ready (Wave 1)
         ↓
Atlas Operator Deployed (Wave 2) ← THIS DOCUMENT
         ↓
AtlasMigration CR Created (per tenant)
         ↓
Atlas Operator Reconciles (30-60s loop)
         ↓
Schema Created: tenant_<id>
         ↓
Baseline Migrations Applied
         ↓
PostgREST Deployed (Wave 3)
         ↓
Tenant Ready for Requests
```

### 1.2 Spec Requirements Mapping

| Requirement | Atlas Responsibility |
|-------------|---------------------|
| **FR-4.1** | Create deterministic schema `tenant_<id>` via AtlasMigration CR |
| **FR-4.4** | Continuous drift detection (Git vs Schema) with 30-60s reconciliation loop |
| **FR-5.2** | GitOps-driven schema provisioning: Git → ArgoCD → Atlas Operator → CNPG |
| **FR-5.3** | Verify schema exists, roles exist, migrations applied via `atlas_schema_revisions` table |
| **AC-5** | Schema provisioning completes within 5 seconds (CREATE SCHEMA + baseline migrations) |
| **NFR-1.3** | Tenant schema provisioning < 5 seconds (GitOps commit → ArgoCD sync → Atlas apply) |
| **NFR-6.1** | All migrations idempotent (`IF NOT EXISTS`, `CREATE OR REPLACE`) |
| **NFR-6.5** | Validate migrations in dev database before production apply |
| **NFR-6.8** | Automatic drift detection and recovery |

---

## 2. Atlas Architecture Overview

### 2.1 Two Components

**Atlas CLI** (declarative migration engine):
- Compares desired state (Git migrations) vs actual state (database schema)
- Generates migration plan (CREATE, ALTER, DROP statements)
- Validates migrations in temporary dev database
- Applies migrations to production database
- Tracks migration history in `atlas_schema_revisions` table

**Atlas Kubernetes Operator** (GitOps reconciler):
- Watches `AtlasMigration` Custom Resources
- Reconciles every 30-60 seconds (controller-runtime default)
- Detects drift: missing migrations, schema changes, manual alterations
- Auto-applies missing migrations when drift detected
- Updates CR status: `Ready=True/False`
- Exposes metrics: `atlas_drift_detected_total`, `atlas_migrations_applied_total`

### 2.2 Versioned Migrations (Spoke Pool Uses This)

**Source**: `archived/atlas/atlas/README.md` - "Versioned Migrations"

**How It Works**:
1. Migrations stored as SQL files in Git: `migrations/tenant-baseline/YYYYMMDDHHMMSS_*.sql`
2. Atlas Operator reads migration directory from ConfigMap or Git
3. Atlas CLI compares migrations vs `atlas_schema_revisions` table for `tenant_<id>` schema
4. Missing migrations are applied in lexicographical order
5. Migration history tracked per schema: `atlas_schema_revisions.schema_name = 'tenant_<id>'`

**Why Versioned (not Declarative)**:
- Deterministic schema naming: `tenant_<id>` enables safe replay
- Idempotent migrations: `CREATE SCHEMA IF NOT EXISTS tenant_<id>`
- Forward-only migrations: No destructive operations without approval
- Audit trail: Every migration file is immutable once merged

---

## 3. AtlasMigration Custom Resource

### 3.1 CRD Spec

**Source**: `archived/atlas/atlas-operator/api/v1alpha1/atlasmigration_types.go`

```yaml
apiVersion: db.atlasgo.io/v1alpha1
kind: AtlasMigration
metadata:
  name: tenant-acme  # One CR per tenant schema
  namespace: spoke-pool-01
spec:
  # Database connection (from Secret)
  urlFrom:
    secretKeyRef:
      name: cnpg-pooler-credentials  # PgBouncer connection string
      key: uri
  
  # Migration directory (from ConfigMap or Git)
  dir:
    configMapRef:
      name: tenant-baseline-migrations
  
  # Dev database for validation (ephemeral container)
  devURL: ""  # Empty = operator spins up temp container
  
  # Schema-specific revisions table
  revisionsSchema: "tenant_acme"  # Tracks migrations for this schema only
  
  # Baseline version (first migration)
  baseline: "20240101000000"
  
  # Execution order
  execOrder: linear  # Apply migrations in order
  
  # Retry policy
  backoffLimit: 20  # Retry 20 times on failure
```

### 3.2 CRD Status

```yaml
status:
  # Reconciliation state
  observedGeneration: 5
  conditions:
    - type: Ready
      status: "True"  # or "False"
      reason: Applied  # or Reconciling, Migrating, etc.
      message: ""
      lastTransitionTime: "2024-03-20T09:59:56Z"
  
  # Migration tracking
  lastAppliedVersion: "20240313121148"  # Latest migration applied
  lastApplied: 1710343398  # Unix timestamp
  observed_hash: "d5a1c1c08de2530d9397d4"  # Hash of migration directory
  
  # Failure tracking
  failed: 0  # Increments on error, resets on success
```

**ArgoCD Health Check**: ArgoCD uses `status.conditions[Ready=True]` to gate sync wave 3 (PostgREST deployment)

---

## 4. GitOps-Driven Schema Provisioning Flow

### 4.1 Universal Tenant Helm Chart Pattern

**FR-5.2 Requirement**: MCP API commits tenant intent (values.yaml), Helm generates CRs, ArgoCD syncs, Atlas Operator applies migrations

```
1. MCP API receives tenant_create(tenant_id="acme", tier="starter")
         ↓
2. MCP API commits values to Git: fleet-registry/tenants/tenant-acme/values.yaml
         ↓
3. ArgoCD ApplicationSet (Git Generator) detects new tenant directory
         ↓
4. ApplicationSet creates Helm Application pointing to Universal Tenant Chart
         ↓
5. Helm renders templates with tenant values:
   - AINativeSaaS XR (tenant namespace, RBAC, ResourceQuota)
   - AtlasMigration CR (schema provisioning)
         ↓
6. ArgoCD deploys generated CRs to Spoke Pool cluster (sync wave 2)
         ↓
7. Atlas Operator reconciles AtlasMigration CR:
   a. Reads migrations from ConfigMap (tenant-baseline-migrations)
   b. Connects to CNPG via PgBouncer (transaction pooling)
   c. Checks atlas_schema_revisions table for tenant_acme schema
   d. Detects missing migrations
   e. Validates migrations in ephemeral dev database
   f. Applies migrations to production: CREATE SCHEMA IF NOT EXISTS tenant_acme
   g. Creates schema owner role: tenant_acme_role
   h. Applies baseline tables with RLS policies
   i. Updates atlas_schema_revisions table
   j. Updates CR status: Ready=True
         ↓
8. ArgoCD health check passes (Ready=True)
         ↓
9. ArgoCD proceeds to sync wave 3 (PostgREST deployment)
         ↓
10. PostgREST deployed with db-schemas including tenant_acme
         ↓
11. Tenant ready for requests via AgentGateway → PostgREST
```

### 4.2 Tenant Values File

**File**: `fleet-registry/tenants/tenant-acme/values.yaml`

```yaml
tenantId: acme
tier: starter
region: hel1

database:
  schemaName: tenant_acme  # Deterministic naming
  migrations:
    gitRepo: https://github.com/kamarkat/kamarkat
    path: migrations/tenant-baseline
    revision: main
  
  # Connection via PgBouncer (transaction pooling)
  pooler:
    enabled: true
    mode: transaction
    maxConnections: 5  # Per tenant limit

# Resource limits (enforced by Helm chart)
resources:
  limits:
    cpu: "500m"
    memory: "512Mi"
  requests:
    cpu: "100m"
    memory: "128Mi"
```

### 4.3 Universal Tenant Helm Chart Template

**File**: `charts/universal-tenant/templates/atlasmigration.yaml`

```yaml
apiVersion: db.atlasgo.io/v1alpha1
kind: AtlasMigration
metadata:
  name: {{ .Values.tenantId }}
  namespace: {{ .Release.Namespace }}
  labels:
    tenant-id: {{ .Values.tenantId }}
    tier: {{ .Values.tier }}
  annotations:
    argocd.argoproj.io/sync-wave: "2"  # After CNPG (wave 1), before PostgREST (wave 3)
spec:
  urlFrom:
    secretKeyRef:
      name: cnpg-pooler-credentials
      key: uri
  
  dir:
    configMapRef:
      name: tenant-baseline-migrations
  
  revisionsSchema: {{ .Values.database.schemaName }}
  baseline: "20240101000000"
  execOrder: linear
  backoffLimit: 20
```

---

## 5. Migration Directory Structure

### 5.1 Git Repository Layout

**Repository**: `https://github.com/kamarkat/kamarkat`

```
migrations/
└── tenant-baseline/
    ├── 20240101000000_init_schema.sql
    ├── 20240101000001_create_users_table.sql
    ├── 20240101000002_create_posts_table.sql
    ├── 20240101000003_enable_rls.sql
    └── atlas.sum  # Checksum file (auto-generated by Atlas)
```

### 5.2 Migration File Format

**Naming Convention**: `YYYYMMDDHHMMSS_description.sql` (NFR-6.6)

**Baseline Source**: Adapted from Supabase auth + storage schemas (`archived/supabase/auth/migrations/`, `archived/supabase/storage/migrations/tenant/`)

**Key Adaptations**:
- Supabase uses database-per-tenant with global `auth` schema
- Our pattern uses schema-per-tenant: `tenant_<id>.auth`, `tenant_<id>.storage`, `tenant_<id>.public`
- Replace Supabase's `{{ index .Options "Namespace" }}` with `tenant_{{ .SchemaName }}`
- All migrations are idempotent (NFR-6.1) and forward-only (NFR-6.2)

---

**Migration 1**: `20240101000000_init_tenant_schema.sql`

```sql
-- Atlas migration: Initialize tenant schema and owner role
-- Source: Custom (platform-specific)
-- Idempotent: safe to replay (NFR-6.1)

-- Create tenant schema (deterministic naming)
CREATE SCHEMA IF NOT EXISTS tenant_{{ .SchemaName }};

-- Create schema owner role
DO $$
BEGIN
  IF NOT EXISTS (SELECT FROM pg_roles WHERE rolname = 'tenant_{{ .SchemaName }}_role') THEN
    CREATE ROLE tenant_{{ .SchemaName }}_role;
  END IF;
END
$$;

-- Grant schema ownership
GRANT ALL ON SCHEMA tenant_{{ .SchemaName }} TO tenant_{{ .SchemaName }}_role;

-- Set default search_path for role
ALTER ROLE tenant_{{ .SchemaName }}_role SET search_path TO tenant_{{ .SchemaName }};

-- Enable pgvector extension (AI features)
CREATE EXTENSION IF NOT EXISTS vector SCHEMA tenant_{{ .SchemaName }};
```

---

**Migration 2**: `20240101000001_auth_users_table.sql`

```sql
-- Atlas migration: Create auth.users table
-- Source: Adapted from archived/supabase/auth/migrations/00_init_auth_schema.up.sql
-- Idempotent: safe to replay (NFR-6.1)

-- Create users table (core identity table)
CREATE TABLE IF NOT EXISTS tenant_{{ .SchemaName }}.users (
  instance_id UUID NULL,
  id UUID NOT NULL DEFAULT gen_random_uuid(),
  aud VARCHAR(255) NULL,
  role VARCHAR(255) NULL,
  email VARCHAR(255) UNIQUE NULL,
  encrypted_password VARCHAR(255) NULL,
  email_confirmed_at TIMESTAMPTZ NULL,
  invited_at TIMESTAMPTZ NULL,
  confirmation_token VARCHAR(255) NULL,
  confirmation_sent_at TIMESTAMPTZ NULL,
  recovery_token VARCHAR(255) NULL,
  recovery_sent_at TIMESTAMPTZ NULL,
  email_change_token_new VARCHAR(255) NULL,
  email_change VARCHAR(255) NULL,
  email_change_sent_at TIMESTAMPTZ NULL,
  last_sign_in_at TIMESTAMPTZ NULL,
  raw_app_meta_data JSONB NULL,
  raw_user_meta_data JSONB NULL,
  is_super_admin BOOLEAN NULL,
  created_at TIMESTAMPTZ NULL DEFAULT NOW(),
  updated_at TIMESTAMPTZ NULL DEFAULT NOW(),
  phone VARCHAR(15) UNIQUE NULL,
  phone_confirmed_at TIMESTAMPTZ NULL,
  phone_change VARCHAR(15) NULL DEFAULT '',
  phone_change_token VARCHAR(255) NULL DEFAULT '',
  phone_change_sent_at TIMESTAMPTZ NULL,
  confirmed_at TIMESTAMPTZ GENERATED ALWAYS AS (LEAST(email_confirmed_at, phone_confirmed_at)) STORED,
  email_change_token_current VARCHAR(255) NULL DEFAULT '',
  email_change_confirm_status SMALLINT NULL DEFAULT 0,
  banned_until TIMESTAMPTZ NULL,
  reauthentication_token VARCHAR(255) NULL DEFAULT '',
  reauthentication_sent_at TIMESTAMPTZ NULL,
  is_sso_user BOOLEAN NOT NULL DEFAULT FALSE,
  deleted_at TIMESTAMPTZ NULL,
  is_anonymous BOOLEAN NOT NULL DEFAULT FALSE,
  CONSTRAINT users_pkey PRIMARY KEY (id)
);

-- Create indexes for performance
CREATE INDEX IF NOT EXISTS users_instance_id_idx ON tenant_{{ .SchemaName }}.users (instance_id);
CREATE INDEX IF NOT EXISTS users_email_idx ON tenant_{{ .SchemaName }}.users (email);
CREATE INDEX IF NOT EXISTS users_phone_idx ON tenant_{{ .SchemaName }}.users (phone);
CREATE INDEX IF NOT EXISTS users_is_anonymous_idx ON tenant_{{ .SchemaName }}.users (is_anonymous);

-- Enable RLS (end-user isolation within tenant schema)
ALTER TABLE tenant_{{ .SchemaName }}.users ENABLE ROW LEVEL SECURITY;

-- RLS policy: users can only see their own data
CREATE POLICY IF NOT EXISTS users_isolation ON tenant_{{ .SchemaName }}.users
  FOR ALL
  TO tenant_{{ .SchemaName }}_role
  USING (id::text = current_setting('request.jwt.claims', true)::json->>'sub');

-- Grant table permissions to schema owner role
GRANT ALL ON TABLE tenant_{{ .SchemaName }}.users TO tenant_{{ .SchemaName }}_role;
```

---

**Migration 3**: `20240101000002_auth_sessions_table.sql`

```sql
-- Atlas migration: Create auth.sessions table
-- Source: Adapted from archived/supabase/auth/migrations/20220811173540_add_sessions_table.up.sql
-- Idempotent: safe to replay (NFR-6.1)

-- Create sessions table
CREATE TABLE IF NOT EXISTS tenant_{{ .SchemaName }}.sessions (
  id UUID NOT NULL DEFAULT gen_random_uuid(),
  user_id UUID NOT NULL,
  created_at TIMESTAMPTZ NULL DEFAULT NOW(),
  updated_at TIMESTAMPTZ NULL DEFAULT NOW(),
  factor_id UUID NULL,
  aal VARCHAR(10) NULL,
  not_after TIMESTAMPTZ NULL,
  refreshed_at TIMESTAMPTZ NULL,
  user_agent TEXT NULL,
  ip INET NULL,
  tag TEXT NULL,
  CONSTRAINT sessions_pkey PRIMARY KEY (id),
  CONSTRAINT sessions_user_id_fkey FOREIGN KEY (user_id) REFERENCES tenant_{{ .SchemaName }}.users(id) ON DELETE CASCADE
);

-- Create indexes
CREATE INDEX IF NOT EXISTS sessions_user_id_idx ON tenant_{{ .SchemaName }}.sessions (user_id);
CREATE INDEX IF NOT EXISTS sessions_not_after_idx ON tenant_{{ .SchemaName }}.sessions (not_after DESC);

-- Enable RLS
ALTER TABLE tenant_{{ .SchemaName }}.sessions ENABLE ROW LEVEL SECURITY;

-- RLS policy: users can only see their own sessions
CREATE POLICY IF NOT EXISTS sessions_isolation ON tenant_{{ .SchemaName }}.sessions
  FOR ALL
  TO tenant_{{ .SchemaName }}_role
  USING (user_id::text = current_setting('request.jwt.claims', true)::json->>'sub');

-- Grant permissions
GRANT ALL ON TABLE tenant_{{ .SchemaName }}.sessions TO tenant_{{ .SchemaName }}_role;
```

---

**Migration 4**: `20240101000003_auth_identities_table.sql`

```sql
-- Atlas migration: Create auth.identities table (OAuth/SSO)
-- Source: Adapted from archived/supabase/auth/migrations/20210909172000_create_identities_table.up.sql
-- Idempotent: safe to replay (NFR-6.1)

-- Create identities table (OAuth providers)
CREATE TABLE IF NOT EXISTS tenant_{{ .SchemaName }}.identities (
  id TEXT NOT NULL,
  user_id UUID NOT NULL,
  identity_data JSONB NOT NULL,
  provider TEXT NOT NULL,
  last_sign_in_at TIMESTAMPTZ NULL,
  created_at TIMESTAMPTZ NULL DEFAULT NOW(),
  updated_at TIMESTAMPTZ NULL DEFAULT NOW(),
  email TEXT GENERATED ALWAYS AS (lower(identity_data->>'email')) STORED,
  CONSTRAINT identities_pkey PRIMARY KEY (id, provider),
  CONSTRAINT identities_user_id_fkey FOREIGN KEY (user_id) REFERENCES tenant_{{ .SchemaName }}.users(id) ON DELETE CASCADE
);

-- Create indexes
CREATE INDEX IF NOT EXISTS identities_user_id_idx ON tenant_{{ .SchemaName }}.identities (user_id);
CREATE INDEX IF NOT EXISTS identities_email_idx ON tenant_{{ .SchemaName }}.identities (email);

-- Enable RLS
ALTER TABLE tenant_{{ .SchemaName }}.identities ENABLE ROW LEVEL SECURITY;

-- RLS policy: users can only see their own identities
CREATE POLICY IF NOT EXISTS identities_isolation ON tenant_{{ .SchemaName }}.identities
  FOR ALL
  TO tenant_{{ .SchemaName }}_role
  USING (user_id::text = current_setting('request.jwt.claims', true)::json->>'sub');

-- Grant permissions
GRANT ALL ON TABLE tenant_{{ .SchemaName }}.identities TO tenant_{{ .SchemaName }}_role;
```

---

**Migration 5**: `20240101000004_storage_buckets_table.sql`

```sql
-- Atlas migration: Create storage.buckets table
-- Source: Adapted from archived/supabase/storage/migrations/tenant/0002-storage-schema.sql
-- Idempotent: safe to replay (NFR-6.1)

-- Create buckets table
CREATE TABLE IF NOT EXISTS tenant_{{ .SchemaName }}.buckets (
  id TEXT NOT NULL,
  name TEXT NOT NULL,
  owner UUID NULL,
  created_at TIMESTAMPTZ NULL DEFAULT NOW(),
  updated_at TIMESTAMPTZ NULL DEFAULT NOW(),
  public BOOLEAN NULL DEFAULT FALSE,
  avif_autodetection BOOLEAN NULL DEFAULT FALSE,
  file_size_limit BIGINT NULL,
  allowed_mime_types TEXT[] NULL,
  owner_id TEXT NULL,
  CONSTRAINT buckets_pkey PRIMARY KEY (id),
  CONSTRAINT buckets_owner_fkey FOREIGN KEY (owner) REFERENCES tenant_{{ .SchemaName }}.users(id) ON DELETE CASCADE
);

-- Create unique index on bucket name
CREATE UNIQUE INDEX IF NOT EXISTS buckets_name_idx ON tenant_{{ .SchemaName }}.buckets (name);

-- Enable RLS
ALTER TABLE tenant_{{ .SchemaName }}.buckets ENABLE ROW LEVEL SECURITY;

-- RLS policy: users can only see their own buckets or public buckets
CREATE POLICY IF NOT EXISTS buckets_isolation ON tenant_{{ .SchemaName }}.buckets
  FOR ALL
  TO tenant_{{ .SchemaName }}_role
  USING (
    public = TRUE OR 
    owner::text = current_setting('request.jwt.claims', true)::json->>'sub'
  );

-- Grant permissions
GRANT ALL ON TABLE tenant_{{ .SchemaName }}.buckets TO tenant_{{ .SchemaName }}_role;
```

---

**Migration 6**: `20240101000005_storage_objects_table.sql`

```sql
-- Atlas migration: Create storage.objects table
-- Source: Adapted from archived/supabase/storage/migrations/tenant/0002-storage-schema.sql
-- Idempotent: safe to replay (NFR-6.1)

-- Create objects table
CREATE TABLE IF NOT EXISTS tenant_{{ .SchemaName }}.objects (
  id UUID NOT NULL DEFAULT gen_random_uuid(),
  bucket_id TEXT NULL,
  name TEXT NULL,
  owner UUID NULL,
  created_at TIMESTAMPTZ NULL DEFAULT NOW(),
  updated_at TIMESTAMPTZ NULL DEFAULT NOW(),
  last_accessed_at TIMESTAMPTZ NULL DEFAULT NOW(),
  metadata JSONB NULL,
  path_tokens TEXT[] GENERATED ALWAYS AS (string_to_array(name, '/')) STORED,
  version TEXT NULL,
  owner_id TEXT NULL,
  user_metadata JSONB NULL,
  CONSTRAINT objects_pkey PRIMARY KEY (id),
  CONSTRAINT objects_bucket_id_fkey FOREIGN KEY (bucket_id) REFERENCES tenant_{{ .SchemaName }}.buckets(id) ON DELETE CASCADE,
  CONSTRAINT objects_owner_fkey FOREIGN KEY (owner) REFERENCES tenant_{{ .SchemaName }}.users(id) ON DELETE CASCADE
);

-- Create indexes
CREATE INDEX IF NOT EXISTS objects_bucket_id_idx ON tenant_{{ .SchemaName }}.objects (bucket_id);
CREATE INDEX IF NOT EXISTS objects_name_idx ON tenant_{{ .SchemaName }}.objects (name);
CREATE INDEX IF NOT EXISTS objects_owner_idx ON tenant_{{ .SchemaName }}.objects (owner);

-- Enable RLS
ALTER TABLE tenant_{{ .SchemaName }}.objects ENABLE ROW LEVEL SECURITY;

-- RLS policy: users can only see their own objects or objects in public buckets
CREATE POLICY IF NOT EXISTS objects_isolation ON tenant_{{ .SchemaName }}.objects
  FOR ALL
  TO tenant_{{ .SchemaName }}_role
  USING (
    bucket_id IN (SELECT id FROM tenant_{{ .SchemaName }}.buckets WHERE public = TRUE) OR
    owner::text = current_setting('request.jwt.claims', true)::json->>'sub'
  );

-- Grant permissions
GRANT ALL ON TABLE tenant_{{ .SchemaName }}.objects TO tenant_{{ .SchemaName }}_role;
```

---

**Migration 7**: `20240101000006_app_posts_table.sql`

```sql
-- Atlas migration: Create application-specific posts table (example)
-- Source: Custom (application-specific)
-- Idempotent: safe to replay (NFR-6.1)

-- Create posts table (example application table)
CREATE TABLE IF NOT EXISTS tenant_{{ .SchemaName }}.posts (
  id UUID NOT NULL DEFAULT gen_random_uuid(),
  user_id UUID NOT NULL,
  title TEXT NOT NULL,
  content TEXT NULL,
  published BOOLEAN NOT NULL DEFAULT FALSE,
  created_at TIMESTAMPTZ NULL DEFAULT NOW(),
  updated_at TIMESTAMPTZ NULL DEFAULT NOW(),
  CONSTRAINT posts_pkey PRIMARY KEY (id),
  CONSTRAINT posts_user_id_fkey FOREIGN KEY (user_id) REFERENCES tenant_{{ .SchemaName }}.users(id) ON DELETE CASCADE
);

-- Create indexes
CREATE INDEX IF NOT EXISTS posts_user_id_idx ON tenant_{{ .SchemaName }}.posts (user_id);
CREATE INDEX IF NOT EXISTS posts_published_idx ON tenant_{{ .SchemaName }}.posts (published);
CREATE INDEX IF NOT EXISTS posts_created_at_idx ON tenant_{{ .SchemaName }}.posts (created_at DESC);

-- Enable RLS
ALTER TABLE tenant_{{ .SchemaName }}.posts ENABLE ROW LEVEL SECURITY;

-- RLS policy: users can only see their own posts or published posts
CREATE POLICY IF NOT EXISTS posts_isolation ON tenant_{{ .SchemaName }}.posts
  FOR ALL
  TO tenant_{{ .SchemaName }}_role
  USING (
    published = TRUE OR
    user_id::text = current_setting('request.jwt.claims', true)::json->>'sub'
  );

-- Grant permissions
GRANT ALL ON TABLE tenant_{{ .SchemaName }}.posts TO tenant_{{ .SchemaName }}_role;
```

---

**Key Design Decisions**:

1. **Schema Naming**: `tenant_{{ .SchemaName }}` (e.g., `tenant_acme`) - deterministic, idempotent
2. **RLS Pattern**: JWT claim `sub` (user_id) for end-user isolation within tenant schema
3. **Supabase Compatibility**: Auth + Storage tables adapted from Supabase baseline
4. **Application Tables**: Example `posts` table shows pattern for custom app tables
5. **Idempotency**: All migrations use `IF NOT EXISTS`, `CREATE OR REPLACE`
6. **Foreign Keys**: Cascade deletes ensure referential integrity
7. **Indexes**: Performance indexes on frequently queried columns
8. **pgvector**: Enabled in schema for AI features (embeddings, similarity search)

### 5.3 ConfigMap for Migration Directory

**File**: `edge-catalog/spoke-pool/atlas/tenant-baseline-migrations-cm.yaml`

```yaml
apiVersion: v1
kind: ConfigMap
metadata:
  name: tenant-baseline-migrations
  namespace: spoke-pool-01
data:
  20240101000000_init_schema.sql: |
    CREATE SCHEMA IF NOT EXISTS tenant_{{ .SchemaName }};
    -- ... (full migration content)
  
  20240101000001_create_users_table.sql: |
    CREATE TABLE IF NOT EXISTS tenant_{{ .SchemaName }}.users (
      -- ... (full migration content)
    );
  
  atlas.sum: |
    h1:FwM0ApKo8xhcZFrSlpa6dYjvi0fnDPo/aZSzajtbHLc=
    20240101000000_init_schema.sql h1:ldFr73m6ZQzNi8q9dVJsOU/ZHmkBo4Sax03AaL0VUUs=
    20240101000001_create_users_table.sql h1:8kQzNi8q9dVJsOU/ZHmkBo4Sax03AaL0VUUs=
```

**ArgoCD Sync Wave**: Wave 2 (after CNPG, before AtlasMigration CRs)

---

## 6. Drift Detection and Reconciliation

### 6.1 Continuous Reconciliation Loop

**FR-4.4 Requirement**: Atlas Operator continuously reconciles Git (desired state) vs Schema (actual state)

```
Every 30-60 seconds (controller-runtime default):
1. Atlas Operator wakes up
2. Reads AtlasMigration CR for tenant_acme
3. Fetches migration directory from ConfigMap
4. Connects to CNPG via PgBouncer
5. Queries atlas_schema_revisions table for tenant_acme schema
6. Compares Git migrations vs applied migrations
7. Detects drift:
   - Missing migrations (new files in Git)
   - Schema changes (manual ALTER TABLE)
   - Deleted migrations (files removed from Git)
8. If drift detected:
   a. Validates missing migrations in ephemeral dev database
   b. Applies missing migrations to production
   c. Updates atlas_schema_revisions table
   d. Updates CR status: Ready=True
   e. Emits metric: atlas_drift_detected_total++
9. If no drift:
   - CR status remains Ready=True
   - No action taken
```

### 6.2 Drift Scenarios

**Scenario 1: New Migration Added**
```
1. Developer adds 20240101000004_add_posts_table.sql to Git
2. ArgoCD syncs ConfigMap (tenant-baseline-migrations updated)
3. Atlas Operator detects new migration file
4. Validates migration in dev database
5. Applies migration to all tenant schemas: tenant_acme, tenant_xyz, etc.
6. Updates atlas_schema_revisions for each schema
7. CR status: Ready=True
```

**Scenario 2: Manual Schema Change (Drift)**
```
1. DBA manually runs: ALTER TABLE tenant_acme.users ADD COLUMN phone VARCHAR(20);
2. Atlas Operator detects drift (schema != Git)
3. Operator logs warning: "Drift detected in tenant_acme schema"
4. Operator does NOT auto-revert (versioned migrations are forward-only)
5. CR status: Ready=False, Reason: DriftDetected
6. Platform Admin must:
   a. Create new migration file: 20240101000005_add_phone_column.sql
   b. Commit to Git
   c. ArgoCD syncs ConfigMap
   d. Atlas Operator applies migration
   e. CR status: Ready=True
```

**Scenario 3: Migration File Deleted (Dangerous)**
```
1. Developer accidentally deletes 20240101000002_create_posts_table.sql from Git
2. ArgoCD syncs ConfigMap (migration file removed)
3. Atlas Operator detects missing migration
4. Operator logs error: "Migration 20240101000002 exists in database but not in Git"
5. CR status: Ready=False, Reason: MigrationMismatch
6. Platform Admin must restore migration file to Git
```

### 6.3 Drift Recovery

**NFR-3.6 Requirement**: Drift recovery completes within 5 minutes of detection (P95)

```
Drift Detected (T+0s)
         ↓
Atlas Operator Reconciles (T+30s)
         ↓
Validates Migration in Dev DB (T+60s)
         ↓
Applies Migration to Production (T+90s)
         ↓
Updates CR Status (T+120s)
         ↓
ArgoCD Health Check Passes (T+150s)
         ↓
Drift Recovered (T+180s = 3 minutes)
```

---

## 7. Dev Database for Migration Validation

### 7.1 Ephemeral Dev Database

**NFR-6.5 Requirement**: Validate migrations in dev database before production apply

**How It Works**:
1. Atlas Operator spins up ephemeral PostgreSQL container (same version as CNPG)
2. Applies all existing migrations to dev database
3. Applies new migration to dev database
4. Validates migration succeeds without errors
5. Destroys ephemeral container
6. Applies migration to production database

**Configuration** (AtlasMigration CR):
```yaml
spec:
  devURL: ""  # Empty = operator spins up temp container
  # OR
  devURL: "docker://postgres/16/dev"  # Explicit dev database
```

### 7.2 Pre-Warm Dev Database

**Optimization**: Atlas Operator keeps dev database containers around to speed up validation

**Helm Chart Configuration** (`values.yaml`):
```yaml
prewarmDevDB: true  # Keep dev containers running (default)
```

**Trade-off**:
- Faster validation (no container startup time)
- Higher memory usage (one dev container per database type)

---

## 8. Migration Safety Checks

### 8.1 Idempotent Migrations

**NFR-6.1 Requirement**: All migrations must be idempotent

**Enforced Patterns**:
```sql
-- ✅ Good: Idempotent
CREATE SCHEMA IF NOT EXISTS tenant_acme;
CREATE TABLE IF NOT EXISTS tenant_acme.users (...);
CREATE INDEX IF NOT EXISTS idx_users_email ON tenant_acme.users(email);

-- ❌ Bad: Not idempotent
CREATE SCHEMA tenant_acme;  -- Fails if schema exists
CREATE TABLE tenant_acme.users (...);  -- Fails if table exists
```

### 8.2 Forward-Only Migrations

**NFR-6.2 Requirement**: All migrations must be forward-only (no destructive operations)

**Enforced Patterns**:
```sql
-- ✅ Good: Forward-only
ALTER TABLE tenant_acme.users ADD COLUMN IF NOT EXISTS phone VARCHAR(20);

-- ❌ Bad: Destructive (requires approval)
ALTER TABLE tenant_acme.users DROP COLUMN email;
DROP TABLE tenant_acme.posts;
```

**Approval Flow** (NFR-6.3):
```yaml
spec:
  protectedFlows:
    migrateDown:
      allow: false  # Destructive operations disabled by default
      autoApprove: false  # Require manual approval
```

### 8.3 Immutable Migration Files

**NFR-6.7 Requirement**: Each migration file is immutable once merged to main branch

**Enforcement**:
- Atlas checksum file (`atlas.sum`) tracks file hashes
- Operator detects modified migration files
- CR status: Ready=False, Reason: MigrationModified
- Platform Admin must create new migration file (not modify existing)

---

## 9. Integration with PostgREST

### 9.1 Schema Discovery

**FR-4.1 Requirement**: PostgREST `db-schemas` config updated to include new tenant schema

**How It Works**:
1. Atlas Operator creates schema: `tenant_acme`
2. Atlas Operator updates CR status: Ready=True
3. ArgoCD health check passes
4. ArgoCD proceeds to sync wave 3 (PostgREST deployment)
5. PostgREST Helm chart template reads all AtlasMigration CRs
6. PostgREST ConfigMap generated with `db-schemas: tenant_acme,tenant_xyz,...`
7. PostgREST deployed with updated config
8. PostgREST health check verifies schema exists before accepting requests

**PostgREST ConfigMap Template**:
```yaml
apiVersion: v1
kind: ConfigMap
metadata:
  name: postgrest-config
  namespace: spoke-pool-01
data:
  postgrest.conf: |
    db-uri = "postgres://pooler@cnpg-pooler:5432/postgres"
    db-schemas = "{{ range .AtlasMigrations }}{{ .Spec.RevisionsSchema }},{{ end }}"
    db-anon-role = "anonymous"
    db-pool = 10
    db-pool-timeout = 10
```

### 9.2 Search Path Routing

**FR-4.2 Requirement**: PostgREST sets `search_path=tenant_<id>` per request

**How It Works**:
1. AgentGateway validates JWT, extracts `tenant_id` claim
2. AgentGateway forwards request to PostgREST with `X-Tenant-ID: acme` header
3. PostgREST reads `X-Tenant-ID` header
4. PostgREST sets `search_path=tenant_acme` for this connection
5. PgBouncer transaction pooling ensures connection reuse without session state leakage
6. PostgreSQL schema isolation prevents cross-tenant access

---

## 10. Design Patterns Alignment

**Source**: sbt-patterns MCP

### 10.1 GitOps-First Pattern
- All schema changes via Git commits (no manual SQL)
- ArgoCD reconciles desired state (migrations in Git)
- Atlas Operator applies migrations to database
- Audit trail via Git history

### 10.2 Declarative Provisioning Pattern
- AtlasMigration CR declares desired schema state
- Atlas Operator converges actual state to desired state
- Idempotent operations (safe to replay)
- No imperative scripts or manual steps

### 10.3 Event-Driven Pattern
- Git commit triggers ArgoCD sync
- ArgoCD sync triggers Atlas Operator reconciliation
- Atlas Operator emits metrics on drift detection
- No polling or blocking operations

### 10.4 Drift Detection Pattern
- Continuous reconciliation (30-60s loop)
- Automatic drift detection (Git vs Schema)
- Automatic drift recovery (apply missing migrations)
- Metrics and logging for observability

---

## 11. Implementation Checklist

### 11.1 Hub Cluster Setup (One-Time)

- [ ] Install Atlas Operator via Helm: `helm install atlas-operator oci://ghcr.io/ariga/charts/atlas-operator`
- [ ] Create migration repository: `migrations/tenant-baseline/`
- [ ] Create baseline migration files (idempotent, forward-only)
- [ ] Generate Atlas checksum file: `atlas migrate hash --dir file://migrations/tenant-baseline`
- [ ] Create ConfigMap: `tenant-baseline-migrations` (ArgoCD sync wave 2)
- [ ] Create Universal Tenant Helm Chart with AtlasMigration template
- [ ] Configure ArgoCD ApplicationSet (Git Generator) for tenant discovery

### 11.2 Per-Cluster Setup (Automated via ArgoCD)

- [ ] Deploy Atlas Operator to Spoke Pool cluster (ArgoCD sync wave 2)
- [ ] Deploy tenant-baseline-migrations ConfigMap (ArgoCD sync wave 2)
- [ ] Deploy CNPG pooler credentials Secret (ArgoCD sync wave 1)
- [ ] Verify Atlas Operator Ready: `kubectl get deployment atlas-operator -n spoke-pool-01`

### 11.3 Per-Tenant Setup (Automated via Helm)

- [ ] MCP API commits tenant values to Git: `fleet-registry/tenants/tenant-acme/values.yaml`
- [ ] ArgoCD ApplicationSet creates Helm Application
- [ ] Helm renders AtlasMigration CR from template
- [ ] ArgoCD deploys AtlasMigration CR (sync wave 2)
- [ ] Atlas Operator reconciles: creates schema, applies migrations
- [ ] Verify schema created: `\dn tenant_acme` in CNPG cluster
- [ ] Verify migrations applied: `SELECT * FROM atlas_schema_revisions WHERE schema_name = 'tenant_acme'`
- [ ] Verify CR status: `kubectl get atlasmigration tenant-acme -o jsonpath='{.status.conditions[?(@.type=="Ready")].status}'`

### 11.4 Verification Commands

```bash
# Check Atlas Operator status
kubectl get pods -n spoke-pool-01 -l app.kubernetes.io/name=atlas-operator

# Check Atlas Operator logs
kubectl logs -n spoke-pool-01 deployment/atlas-operator

# Check AtlasMigration CR status
kubectl get atlasmigration -n spoke-pool-01

# Check AtlasMigration CR details
kubectl describe atlasmigration tenant-acme -n spoke-pool-01

# Check schema exists in CNPG
kubectl exec -it cnpg-cluster-1 -n spoke-pool-01 -- psql -U postgres -c "\dn tenant_acme"

# Check migrations applied
kubectl exec -it cnpg-cluster-1 -n spoke-pool-01 -- psql -U postgres -c "SELECT * FROM atlas_schema_revisions WHERE schema_name = 'tenant_acme'"

# Check schema owner role
kubectl exec -it cnpg-cluster-1 -n spoke-pool-01 -- psql -U postgres -c "\du tenant_acme_role"

# Check baseline tables
kubectl exec -it cnpg-cluster-1 -n spoke-pool-01 -- psql -U postgres -c "\dt tenant_acme.*"
```

---

## 12. Troubleshooting Guide

### 12.1 AtlasMigration CR Not Ready

**Symptom**: `kubectl get atlasmigration tenant-acme` shows `Ready=False`

**Checks**:
```bash
# Check CR status reason
kubectl get atlasmigration tenant-acme -o jsonpath='{.status.conditions[?(@.type=="Ready")].reason}'

# Check CR status message
kubectl get atlasmigration tenant-acme -o jsonpath='{.status.conditions[?(@.type=="Ready")].message}'

# Check Atlas Operator logs
kubectl logs -n spoke-pool-01 deployment/atlas-operator | grep tenant-acme
```

**Common Reasons**:
- `Reconciling`: Operator is applying migrations (wait 30-60s)
- `GettingDevDB`: Failed to spin up dev database container
- `ReadingMigrationData`: ConfigMap not found or invalid
- `Migrating`: Migration failed (check logs for SQL error)

### 12.2 Migration Fails to Apply

**Symptom**: Atlas Operator logs show SQL error

**Checks**:
```bash
# Check migration file syntax
cat migrations/tenant-baseline/20240101000001_create_users_table.sql

# Test migration in local PostgreSQL
psql -U postgres -d test -f migrations/tenant-baseline/20240101000001_create_users_table.sql

# Check CNPG cluster status
kubectl get cluster shared-cnpg -n spoke-pool-01 -o jsonpath='{.status.phase}'

# Check PgBouncer pooler status
kubectl get service cnpg-pooler -n spoke-pool-01
```

**Common Causes**:
- SQL syntax error in migration file
- Missing schema or table dependency
- CNPG cluster not ready
- PgBouncer connection limit reached

### 12.3 Drift Not Detected

**Symptom**: Manual schema change not detected by Atlas Operator

**Checks**:
```bash
# Check Atlas Operator reconciliation interval
kubectl logs -n spoke-pool-01 deployment/atlas-operator | grep "Reconciling AtlasMigration"

# Check AtlasMigration CR observed hash
kubectl get atlasmigration tenant-acme -o jsonpath='{.status.observed_hash}'

# Check ConfigMap hash
kubectl get configmap tenant-baseline-migrations -o yaml | sha256sum

# Force reconciliation
kubectl annotate atlasmigration tenant-acme reconcile=true --overwrite
```

**Common Causes**:
- Reconciliation loop not running (operator crashed)
- ConfigMap not updated (ArgoCD sync failed)
- Migration file hash unchanged (no new migrations)

---

## 13. Acceptance Criteria Validation

| AC | Requirement | Atlas Implementation |
|----|-------------|---------------------|
| **AC-5** | Schema migrations stored in Git | ✅ `migrations/tenant-baseline/YYYYMMDDHHMMSS_*.sql` |
| **AC-5** | Migration files follow Atlas naming | ✅ `YYYYMMDDHHMMSS_description.sql` |
| **AC-5** | Atlas Operator creates deterministic schema | ✅ `tenant_<tenant-id>` (e.g., `tenant_acme`) |
| **AC-5** | Atlas Operator applies baseline migrations | ✅ Reads from ConfigMap, applies to CNPG |
| **AC-5** | Schema owner role created | ✅ `tenant_<tenant-id>_role` |
| **AC-5** | Migration history tracked | ✅ `atlas_schema_revisions` table per schema |
| **AC-5** | AtlasMigration CR status updates | ✅ `Ready=True` after successful apply |
| **AC-5** | ArgoCD health check verifies CR status | ✅ Blocks sync wave 3 if not Ready |
| **AC-5** | Schema provisioning < 5 seconds | ✅ CREATE SCHEMA + baseline migrations |
| **AC-5** | Atlas Operator reconciles drift | ✅ 30-60s loop, auto-applies missing migrations |

---

**Document Version**: 1.0  
**Last Updated**: 2026-04-08  
**Next Steps**: Proceed to PostgREST + AgentGateway integration analysis (FR-2.6, FR-4.5)
