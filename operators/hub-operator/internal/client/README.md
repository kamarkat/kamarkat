# Client Package

This package implements external service clients for the Hub Operator.

## InfisicalClient

The `InfisicalClient` handles secret backup operations to Infisical using Universal Auth.

### Key Features

- **Universal Auth**: Authenticates using client credentials from `infisical-auth` secret (Requirement 6.4)
- **Token Caching**: Caches access token and refreshes before expiration (Requirement 6.5)
- **CreateOrUpdateSecret**: Idempotent secret upload to Infisical API (Requirement 6.6)
- **Retry Logic**: Retries on 5xx errors for transient failures (Requirement 6.7)
- **30 Second Timeout**: All HTTP requests timeout after 30 seconds (Requirement 6.8)
- **Controller-Runtime Integration**: Uses controller-runtime client for Kubernetes operations (Requirement 6.3)

### Usage

```go
import (
    "context"
    "github.com/kamarkat/kamarkat/operators/hub-operator/internal/client"
    "sigs.k8s.io/controller-runtime/pkg/client"
)

// Create Infisical client
infisicalClient, err := client.NewInfisicalClient(ctx, k8sClient, "https://infisical.nutgraf.in")
if err != nil {
    return err
}

// Upload secret (idempotent)
if err := infisicalClient.CreateOrUpdateSecret(ctx, hubEnv, "db-password", "secret123"); err != nil {
    return err
}
```

### Authentication Flow

1. Read `infisical-auth` secret from `platform-ops` namespace
2. Extract `client-id` and `client-secret` from secret data
3. POST to `/api/v1/auth/universal-auth/login` with credentials
4. Cache access token with expiration time
5. Refresh token automatically when expiring within 5 minutes

### Secret Upload Flow

1. Ensure valid authentication token (refresh if needed)
2. Convert `projectSlug` to `workspaceId` via Infisical API
3. Check if secret exists using GET `/api/v3/secrets/raw/{key}`
4. If exists: PATCH `/api/v3/secrets/raw/{key}` to update
5. If not exists: POST `/api/v3/secrets/raw/{key}` to create

### Error Handling

**Transient Errors (5xx)**:
- Authentication failures with status >= 500
- Workspace lookup failures with status >= 500
- Secret existence check failures with status >= 500
- Create/update failures with status >= 500
- All return error with "(transient error)" suffix for controller retry

**Permanent Errors**:
- Missing `infisical-auth` secret
- Invalid credentials (4xx responses)
- Workspace not found
- Malformed API responses

### Configuration

Infisical configuration is read from HubEnvironment CR:

```yaml
spec:
  secrets:
    infisical:
      projectSlug: "platform"
      environmentSlug: "prod"
```

### Security Considerations

- Credentials read from Kubernetes secret (never hardcoded)
- Access token cached in memory only (not persisted)
- Token refreshed before expiration (5 minute buffer)
- All requests use HTTPS
- 30 second timeout prevents hanging connections

### API Endpoints

- **Authentication**: `POST /api/v1/auth/universal-auth/login`
- **List Workspaces**: `GET /api/v1/workspace`
- **Get Secret**: `GET /api/v3/secrets/raw/{key}?workspaceId={id}&environment={env}&secretPath={path}`
- **Create Secret**: `POST /api/v3/secrets/raw/{key}`
- **Update Secret**: `PATCH /api/v3/secrets/raw/{key}`

### Requirement 9 Compliance

The client supports uploading all required secrets per Requirement 9:
- Database usernames and passwords (AC 9.1-9.16)
- Only uploads passwords/keys, not URLs/hostnames (AC 9.23-9.24)
- Tracks uploaded secrets in HubEnvironment status (AC 9.17-9.19)
- Idempotent uploads prevent overwrites (AC 9.19)

## OAuth Client Registration

OAuth client registration is handled by Zitadel (ADR-060). The hub-operator no
longer registers clients directly — Zitadel generates client secrets itself and
discloses them once. The Tenant Identity Service captures and publishes them.

