# Database Package

Names where each hub application login role's credential is delivered. Nothing
here connects to PostgreSQL (ADR-117 §7.3–7.4).

| Concern | Owner |
|---|---|
| The physical cluster, its CA and its superuser credential | CloudNativePG |
| `hub_<app>`: existence, `inRoles: [<app>_owner]`, client certificate | CloudNativePG `DatabaseRole` (no `passwordSecret`) |
| `hub_<app>` password: generation and rotation | OpenBao static role `hub-<app>`, authenticating as `hub_<app>` by certificate |
| The static role's existence | hub-operator (`internal/openbao`, from `HubEnvironment.spec.database.roles`) |
| Delivery to the application | ESO `VaultDynamicSecret` beside the consumer (`DeliveredSecret`) |
| Schema | Each application, at its own cluster (ADR-074) |
