# AWS Credentials Injection Pattern for Hub-Operator

## Document Purpose
This document analyzes how the hub-operator currently handles secret injection and proposes a consistent pattern for AWS Secrets Manager credentials.

---

## Current Pattern Analysis: GitHub Secrets

### Flow Overview
```
CLI Command (hub configure-eso)
  ↓
Creates K8s Secret via client-go (Secret Zero)
  ↓
Hub-Operator reads Secret → Uploads to Infisical
  ↓
ExternalSecret syncs from Infisical → Recreates K8s Secret (GitOps managed)
```

### Implementation Details

#### 1. CLI Command Entry Point
**File**: `zero-ops/cmd/hub/configure_eso.go`

```go
func newConfigureESOCmd() *cobra.Command {
    cmd := &cobra.Command{
        Use:   "configure-eso",
        Short: "Configure External Secrets Operator authentication",
        // Injects Secret Zero credentials for GitOps workflow
    }
    
    cmd.Flags().StringVar(&ghcrPAT, "ghcr-pat", "", "GitHub Personal Access Token")
    cmd.Flags().StringVar(&ghcrUsername, "ghcr-username", "", "GHCR username")
    
    return cmd
}
```

#### 2. Secret Creation via Client-Go
**File**: `zero-ops/internal/hub/components/secrets.go`

```go
func (i *Installer) FixArgoCDGitHubAuth(ctx context.Context, githubToken string) error {
    // Load kubeconfig and create clientset
    config, err := clientcmd.BuildConfigFromFlags("", i.Kubeconfig)
    clientset, err := kubernetes.NewForConfig(config)
    
    // Create the secret with ArgoCD auto-discovery label
    secret := &corev1.Secret{
        ObjectMeta: metav1.ObjectMeta{
            Name:      "platform-git-secret",
            Namespace: constants.NamespaceOps,
            Labels: map[string]string{
                "argocd.argoproj.io/secret-type": "repository",
                "app.kubernetes.io/managed-by":   "zero-ops-hub-cli",
                "app.kubernetes.io/component":    "secret-zero",
            },
        },
        Type: corev1.SecretTypeOpaque,
        StringData: map[string]string{
            "type":     "git",
            "url":      "https://github.com/kamarkat/kamarkat",
            "username": "zero-ops-bot",
            "password": githubToken,
        },
    }
    
    // Create or update
    clientset.CoreV1().Secrets(constants.NamespaceOps).Create(ctx, secret, metav1.CreateOptions{})
}
```

#### 3. Operator Uploads to Infisical
**File**: `zero-ops/operators/hub-operator/internal/controller/hubenvironment_controller.go`

```go
// Called during HubEnvironment reconciliation
secretUploader := infisical.NewSecretUploader(r.Client, r.UncachedClient)
if err := secretUploader.UploadCLISecrets(ctx); err != nil {
    logger.Error(err, "Failed to upload CLI secrets, continuing...")
}
```

**File**: `zero-ops/operators/hub-operator/internal/infisical/secret_uploader.go`

```go
func (su *SecretUploader) UploadCLISecrets(ctx context.Context) error {
    // Create Infisical client
    infisicalClient, err := infisicalclient.NewInfisicalClient(ctx, su.uncachedK8sClient, "")
    
    // Upload all configured secrets from the registry
    mappings := CLISecretMappings
    for _, mapping := range mappings {
        su.uploadSecret(ctx, infisicalClient, projectSlug, environmentSlug, secretPath, mapping)
    }
}

func (su *SecretUploader) uploadSecret(..., mapping SecretMapping) error {
    // Read source secret from K8s (MUST use uncached client)
    secret := &corev1.Secret{}
    su.uncachedK8sClient.Get(ctx, client.ObjectKey{
        Name:      mapping.SourceName,
        Namespace: mapping.SourceNamespace,
    }, secret)
    
    // Extract the specified key
    value := secret.Data[mapping.SourceKey]
    
    // Upload to Infisical
    infisicalClient.CreateOrUpdateSecretRaw(ctx, projectSlug, environmentSlug, 
        secretPath, mapping.InfisicalKey, string(value))
}
```

#### 4. Secret Mapping Registry
**File**: `zero-ops/operators/hub-operator/internal/infisical/secret_mappings.go`

```go
var CLISecretMappings = []SecretMapping{
    // GitHub username for ArgoCD
    {
        SourceNamespace: NamespaceOps,              // platform-ops
        SourceName:      "platform-git-secret", // platform-git-secret
        SourceKey:       "username",                // username
        InfisicalKey:    "github-username",         // github-username
        Description:     "GitHub username for ArgoCD",
    },
    // GitHub token for ArgoCD
    {
        SourceNamespace: NamespaceOps,              // platform-ops
        SourceName:      "platform-git-secret", // platform-git-secret
        SourceKey:       "password",                // password
        InfisicalKey:    "github-token",            // github-token
        Description:     "GitHub token for ArgoCD",
    },
}
```

#### 5. ExternalSecret Syncs from Infisical
**File**: `zero-ops/manifests/hub-core-services/platform-argocd-github-auth/external-secret.yaml`

```yaml
apiVersion: external-secrets.io/v1
kind: ExternalSecret
metadata:
  name: argocd-github-creds
  namespace: platform-ops
  annotations:
    argocd.argoproj.io/sync-wave: "6"
spec:
  refreshInterval: 1h
  
  secretStoreRef:
    name: infisical-backend
    kind: ClusterSecretStore
  
  target:
    name: platform-git-secret
    creationPolicy: Owner
    template:
      metadata:
        labels:
          argocd.argoproj.io/secret-type: repository
      type: Opaque
      data:
        type: git
        url: https://github.com/kamarkat/kamarkat
        username: "{{ .username }}"
        password: "{{ .password }}"
  
  data:
    - secretKey: username
      remoteRef:
        key: github-username
    - secretKey: password
      remoteRef:
        key: github-token
```

---

## Key Principles

### 1. Secret Zero Pattern
- **Secret Zero**: Initial secrets injected via CLI before GitOps takes over
- **Purpose**: Bootstrap the GitOps workflow itself
- **Lifecycle**: CLI creates → Operator uploads to Infisical → ESO manages thereafter

### 2. Infisical as Source of Truth
- All secrets ultimately stored in Infisical
- K8s secrets are ephemeral, recreated from Infisical
- Enables centralized secret rotation and management

### 3. UncachedClient Requirement
**CRITICAL**: Must use `UncachedClient` when reading secrets
- Cached client strips `.Data` payloads for memory optimization
- Will return blank values if using cached client
- See: `hubenvironment_controller.go` line 121

### 4. GitOps Compliance
- After initial bootstrap, ExternalSecret manages the secret
- Changes to secrets happen in Infisical, not K8s directly
- ArgoCD syncs ExternalSecret manifests

---

## Archived Project Pattern (Reference Only)

### Static Keys in Hetzner S3 (Archived Project)
**File**: `zero-ops/archived/zero/zerotouch-platform/scripts/bootstrap/helpers/s3-helpers.sh`

```bash
configure_s3_credentials() {
    export AWS_ACCESS_KEY_ID="${HETZNER_S3_ACCESS_KEY:-}"
    export AWS_SECRET_ACCESS_KEY="${HETZNER_S3_SECRET_KEY:-}"
    export AWS_DEFAULT_REGION="${HETZNER_S3_REGION:-}"
}
```

**File**: `zero-ops/archived/zero/zerotouch-platform/scripts/bootstrap/infra/secrets/03-bootstrap-storage.sh`

```bash
# Configure AWS CLI for Hetzner Object Storage
export AWS_ACCESS_KEY_ID="$HETZNER_S3_ACCESS_KEY"
export AWS_SECRET_ACCESS_KEY="$HETZNER_S3_SECRET_KEY"

# Create Kubernetes secret
kubectl create secret generic hetzner-s3-credentials \
    --namespace=default \
    --from-literal=access-key="$HETZNER_S3_ACCESS_KEY" \
    --from-literal=secret-key="$HETZNER_S3_SECRET_KEY" \
    --from-literal=endpoint="$HETZNER_ENDPOINT" \
    --from-literal=region="$HETZNER_REGION" \
    --dry-run=client -o yaml | kubectl apply -f -
```

**Key Finding**: Archived project used bash scripts to inject static AWS keys into K8s secrets. This is **reference implementation only** - production should use operator-driven pattern.

---

## Proposed Pattern: AWS Secrets Manager Credentials

### Implementation Plan

#### 1. New CLI Command
**File**: `zero-ops/cmd/hub/configure_aws_secrets_manager.go` (NEW)

```go
func newConfigureAWSSecretsManagerCmd() *cobra.Command {
    cmd := &cobra.Command{
        Use:   "configure-aws-secrets-manager",
        Short: "Configure AWS Secrets Manager credentials for hub-operator",
        Long: `Inject AWS IAM credentials for hub-operator to access AWS Secrets Manager.

This enables disaster recovery by backing up Infisical master keys to AWS.

Prerequisites:
- AWS IAM user created with Secrets Manager permissions
- Access keys generated for the IAM user

After running this command:
- hub-operator will have AWS credentials
- Infisical ENCRYPTION_KEY and AUTH_SECRET will be backed up to AWS
- Disaster recovery workflow will be enabled`,
        RunE: runConfigureAWSSecretsManager,
    }

    cmd.Flags().StringVar(&awsAccessKeyID, "aws-access-key-id", "", "AWS Access Key ID")
    cmd.Flags().StringVar(&awsSecretAccessKey, "aws-secret-access-key", "", "AWS Secret Access Key")
    cmd.Flags().StringVar(&awsRegion, "aws-region", "ap-south-1", "AWS Region")
    cmd.Flags().StringVar(&kubeconfig, "kubeconfig", "", "Path to kubeconfig")

    cmd.MarkFlagRequired("aws-access-key-id")
    cmd.MarkFlagRequired("aws-secret-access-key")

    return cmd
}

func runConfigureAWSSecretsManager(cmd *cobra.Command, args []string) error {
    ctx := cmd.Context()

    fmt.Println("🔐 Configuring AWS Secrets Manager credentials...")

    installer := &components.Installer{
        Kubeconfig: kubeconfig,
    }

    if err := installer.InstallAWSSecretsManagerAuth(ctx, awsAccessKeyID, awsSecretAccessKey, awsRegion); err != nil {
        return fmt.Errorf("failed to create AWS credentials secret: %w", err)
    }

    fmt.Println("\n✅ Configuration complete!")
    fmt.Println("\nNext steps:")
    fmt.Println("1. Hub-operator will automatically upload credentials to Infisical")
    fmt.Println("2. Verify secret exists:")
    fmt.Println("   kubectl get secret hub-operator-aws-credentials -n platform-ops")
    fmt.Println("3. Check operator logs for AWS backup confirmation")

    return nil
}
```

#### 2. Secret Creation Implementation
**File**: `zero-ops/internal/hub/components/secrets.go` (ADD)

```go
func (i *Installer) InstallAWSSecretsManagerAuth(ctx context.Context, accessKeyID, secretAccessKey, region string) error {
    fmt.Println("[bootstrap] Creating AWS Secrets Manager credentials...")

    // Load kubeconfig and create clientset
    config, err := clientcmd.BuildConfigFromFlags("", i.Kubeconfig)
    if err != nil {
        return fmt.Errorf("failed to load kubeconfig: %w", err)
    }

    clientset, err := kubernetes.NewForConfig(config)
    if err != nil {
        return fmt.Errorf("failed to create kubernetes client: %w", err)
    }

    // Create the secret with proper labels
    secret := &corev1.Secret{
        ObjectMeta: metav1.ObjectMeta{
            Name:      "hub-operator-aws-credentials",
            Namespace: constants.NamespaceOps,
            Labels: map[string]string{
                "app.kubernetes.io/managed-by": "zero-ops-hub-cli",
                "app.kubernetes.io/component":  "secret-zero",
                "app.kubernetes.io/part-of":    "hub-operator",
            },
        },
        Type: corev1.SecretTypeOpaque,
        StringData: map[string]string{
            "AWS_ACCESS_KEY_ID":     accessKeyID,
            "AWS_SECRET_ACCESS_KEY": secretAccessKey,
            "AWS_REGION":            region,
        },
    }

    // Try to create, if exists then update
    _, err = clientset.CoreV1().Secrets(constants.NamespaceOps).Create(ctx, secret, metav1.CreateOptions{})
    if err != nil {
        // Secret might already exist, try to update
        _, err = clientset.CoreV1().Secrets(constants.NamespaceOps).Update(ctx, secret, metav1.UpdateOptions{})
        if err != nil {
            return fmt.Errorf("failed to create or update AWS credentials secret: %w", err)
        }
        fmt.Println("[bootstrap] ✓ AWS credentials secret updated")
    } else {
        fmt.Println("[bootstrap] ✓ AWS credentials secret created")
    }

    fmt.Println("[bootstrap] Note: Hub-operator will upload credentials to Infisical")
    return nil
}
```

#### 3. Secret Mapping Registry
**File**: `zero-ops/operators/hub-operator/internal/infisical/secret_mappings.go` (ADD)

```go
var CLISecretMappings = []SecretMapping{
    // ... existing mappings ...
    
    // ========================================================================
    // AWS SECRETS MANAGER CREDENTIALS (Disaster Recovery)
    // ========================================================================
    // Purpose: Enable hub-operator to backup Infisical master keys to AWS
    // Source: hub-operator-aws-credentials (deployed by CLI)
    // Consumers: hub-operator
    // ExternalSecret: manifests/hub-operator/aws-credentials-externalsecret.yaml
    {
        SourceNamespace: NamespaceOps,                      // platform-ops
        SourceName:      "hub-operator-aws-credentials",    // hub-operator-aws-credentials
        SourceKey:       "AWS_ACCESS_KEY_ID",               // AWS_ACCESS_KEY_ID
        InfisicalKey:    "aws-access-key-id",               // aws-access-key-id
        Description:     "AWS Access Key ID for Secrets Manager",
    },
    {
        SourceNamespace: NamespaceOps,                      // platform-ops
        SourceName:      "hub-operator-aws-credentials",    // hub-operator-aws-credentials
        SourceKey:       "AWS_SECRET_ACCESS_KEY",           // AWS_SECRET_ACCESS_KEY
        InfisicalKey:    "aws-secret-access-key",           // aws-secret-access-key
        Description:     "AWS Secret Access Key for Secrets Manager",
    },
    {
        SourceNamespace: NamespaceOps,                      // platform-ops
        SourceName:      "hub-operator-aws-credentials",    // hub-operator-aws-credentials
        SourceKey:       "AWS_REGION",                      // AWS_REGION
        InfisicalKey:    "aws-region",                      // aws-region
        Description:     "AWS Region for Secrets Manager",
    },
}
```

#### 4. Deployment Manifest Update
**File**: `zero-ops/operators/hub-operator/config/manager/manager.yaml` (MODIFY)

```yaml
spec:
  template:
    spec:
      containers:
      - name: manager
        env:
        - name: INFISICAL_BASE_URL
          value: "https://infisical.platform-ops.svc"
        - name: HYDRA_BASE_URL
          value: "https://hydra-admin.ory-system.svc"
        - name: NATS_URL
          value: "nats://nats.platform-core.svc:4222"
        # AWS Secrets Manager credentials for disaster recovery
        - name: AWS_ACCESS_KEY_ID
          valueFrom:
            secretKeyRef:
              name: hub-operator-aws-credentials
              key: AWS_ACCESS_KEY_ID
        - name: AWS_SECRET_ACCESS_KEY
          valueFrom:
            secretKeyRef:
              name: hub-operator-aws-credentials
              key: AWS_SECRET_ACCESS_KEY
        - name: AWS_REGION
          valueFrom:
            secretKeyRef:
              name: hub-operator-aws-credentials
              key: AWS_REGION
```

#### 5. ExternalSecret Manifest
**File**: `zero-ops/manifests/hub-operator/aws-credentials-externalsecret.yaml` (NEW)

```yaml
apiVersion: external-secrets.io/v1
kind: ExternalSecret
metadata:
  name: hub-operator-aws-credentials
  namespace: platform-ops
  annotations:
    argocd.argoproj.io/sync-wave: "1"
spec:
  refreshInterval: 1h
  
  secretStoreRef:
    name: infisical-backend
    kind: ClusterSecretStore
  
  target:
    name: hub-operator-aws-credentials
    creationPolicy: Owner
    template:
      metadata:
        labels:
          app.kubernetes.io/managed-by: external-secrets
          app.kubernetes.io/part-of: hub-operator
      type: Opaque
  
  data:
    - secretKey: AWS_ACCESS_KEY_ID
      remoteRef:
        key: aws-access-key-id
    - secretKey: AWS_SECRET_ACCESS_KEY
      remoteRef:
        key: aws-secret-access-key
    - secretKey: AWS_REGION
      remoteRef:
        key: aws-region
```

---

## Usage Workflow

### Developer Workflow

```bash
# Step 1: Create AWS IAM user and policy (one-time setup)
# This can be done manually via AWS Console or using the reference script
# Reference: zero-ops/scripts/aws/02-setup-hub-operator-secrets-manager.sh

# Step 2: Inject AWS credentials via CLI
./bin/hub configure-aws-secrets-manager \
  --aws-access-key-id="AKIA..." \
  --aws-secret-access-key="secret..." \
  --aws-region="ap-south-1" \
  --kubeconfig="~/.kube/config"

# Step 3: Verify secret created
kubectl get secret hub-operator-aws-credentials -n platform-ops

# Step 4: Hub-operator automatically:
# - Reads hub-operator-aws-credentials secret
# - Uploads to Infisical (makes Infisical Source of Truth)
# - Uses AWS SDK to backup Infisical master keys

# Step 5: ArgoCD syncs ExternalSecret manifest
# - ExternalSecret recreates hub-operator-aws-credentials from Infisical
# - Future credential rotations happen in Infisical
```

### Disaster Recovery Workflow

```bash
# When Infisical master keys are lost:

# 1. Hub-operator detects missing keys
# 2. Retrieves keys from AWS Secrets Manager using credentials
# 3. Recreates infisical-master-keys secret
# 4. Restarts Infisical pods
# 5. System recovers automatically
```

---

## Benefits of This Approach

### 1. Consistency
- Follows exact same pattern as GitHub secrets
- Developers already familiar with `hub configure-*` commands
- Same code paths, same testing, same documentation

### 2. GitOps Compliance
- ExternalSecret manages the secret after initial bootstrap
- Changes to AWS credentials happen in Infisical, not K8s directly
- ArgoCD syncs ExternalSecret manifests

### 3. Operator-Driven
- No bash scripts in production
- Operator handles all secret lifecycle operations
- Centralized logic in Go code

### 4. Infisical as Source of Truth
- AWS credentials stored in Infisical after initial injection
- Enables centralized credential rotation
- Audit trail in Infisical

### 5. Disaster Recovery Ready
- Credentials available for key recovery operations
- AWS SDK automatically picks up environment variables
- No additional configuration needed in Go code

---

## What to Remove

### Delete (Reference Implementation Only)
- `zero-ops/scripts/aws/02-setup-hub-operator-secrets-manager.sh`
  - This was created as reference implementation
  - Production uses CLI command instead
  - Can be kept as documentation/example

### Keep (Infrastructure Provisioning)
- `zero-ops/scripts/aws/00-create-oidc-bucket.sh`
- `zero-ops/scripts/aws/01-setup-aws-identity.sh`
- These handle AWS infrastructure setup, not secret injection

---

## Implementation Checklist

- [ ] Create `cmd/hub/configure_aws_secrets_manager.go`
- [ ] Add `InstallAWSSecretsManagerAuth()` to `internal/hub/components/secrets.go`
- [ ] Add AWS credential mappings to `internal/infisical/secret_mappings.go`
- [ ] Update `config/manager/manager.yaml` with AWS env vars
- [ ] Create `manifests/hub-operator/aws-credentials-externalsecret.yaml`
- [ ] Register command in `cmd/hub/main.go`
- [ ] Add AWS SDK dependency to `go.mod`
- [ ] Update requirements.md with CLI command usage
- [ ] Update design.md with implementation details
- [ ] Test end-to-end workflow

---

## References

### Codebase Files Analyzed
- `zero-ops/cmd/hub/configure_eso.go` - CLI command pattern
- `zero-ops/internal/hub/components/secrets.go` - Secret creation via client-go
- `zero-ops/operators/hub-operator/internal/controller/hubenvironment_controller.go` - Operator reconciliation
- `zero-ops/operators/hub-operator/internal/infisical/secret_uploader.go` - Upload to Infisical
- `zero-ops/operators/hub-operator/internal/infisical/secret_mappings.go` - Secret registry
- `zero-ops/manifests/hub-core-services/platform-argocd-github-auth/external-secret.yaml` - ExternalSecret pattern
- `zero-ops/archived/zero/zerotouch-platform/scripts/bootstrap/helpers/s3-helpers.sh` - Archived reference
- `zero-ops/archived/zero/zerotouch-platform/scripts/bootstrap/infra/secrets/03-bootstrap-storage.sh` - Archived reference

### Key Insights
1. **UncachedClient is mandatory** when reading secrets (cached client strips `.Data`)
2. **Secret Zero pattern** enables GitOps bootstrap
3. **Infisical as Source of Truth** ensures centralized management
4. **ExternalSecret recreates secrets** from Infisical after initial bootstrap
5. **Operator-driven approach** eliminates bash script dependencies
