# Database Package

Role provisioning for the hub-operator, against the CNPG clusters a `HubEnvironment`
declares.

## What owns what

| Concern | Owner |
|---|---|
| The physical cluster, its CA and its superuser credential | CloudNativePG |
| Infisical's bootstrap role and database | CloudNativePG, `spec.managed.roles` (ADR-023 addendum) |
| Application roles, their credentials, rotation and pruning | `RoleManager` here, until Crossplane `provider-sql` is deployed (ADR-023) |
| Schema | Each application, at its own cluster (ADR-074, amended 2026-11-03) |

Nothing in this package migrates schema. The only DDL it issues is the grants a role
needs on a database its application owns.

## Endpoint (`endpoint.go`)

One cluster name yields the three object names CloudNativePG derives from it — the
superuser Secret, the CA Secret and the read-write service. Spelling them separately is
how two of them come to point at different clusters, and `Validate` refuses an endpoint
with no cluster rather than assuming one.

## RoleManager (`roles.go`)

- Idempotent creation, with `COMMENT ON ROLE` as the mark of a managed role
- Password drift detection and `ALTER ROLE` rotation
- Grants from the CR spec, applied on the target database over a second connection
  derived from the same `Endpoint`
- Pruning of roles the CR no longer declares

One manager per cluster. `Spec.RolesFor(cluster)` gives it the roles that live there, so
a role is never provisioned against another application's database:

```go
rm, err := database.NewRoleManager(ctx, k8sClient, database.Endpoint{
    Cluster:   cluster,
    Namespace: hubEnv.Spec.Database.Namespace,
})
if err != nil {
    return err
}
defer rm.Close()

if err := rm.CreateOrUpdateRoles(ctx, hubEnv); err != nil {
    return err
}
```

`RoleSecretName` is exported because two things must agree on it: this package applies
the credential, and the controller waits for ESO to deliver it before provisioning any
role. A gate that listed its Secrets separately would drift from the roles it guards.

## SQL notes

PostgreSQL has no inline comment in `CREATE ROLE`, so the mark is a second statement:

```sql
CREATE ROLE username WITH LOGIN PASSWORD 'password';
COMMENT ON ROLE username IS 'Managed by hub-operator';
```

Managed roles are then identified from `pg_shdescription`:

```sql
SELECT r.rolname
FROM pg_roles r
JOIN pg_shdescription d ON r.oid = d.objoid
WHERE d.description = 'Managed by hub-operator'
```

## Security

- Every query is parameterised; identifiers are validated against the CR's own pattern
- Passwords are never logged, nor included in an error
- TLS is required on both connections (`sslmode=require`)
- Superuser credentials are read from the cluster's own CNPG Secret, uncached
