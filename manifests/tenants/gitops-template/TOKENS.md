# Template tokens

Substituted when this template is rendered into a tenant's `<tenant>-gitops`
repository (ADR-062). Every value is an identifier or a location.

**No token carries secret material.** Secrets are generated into, and delivered
from, the tenant's own store; a token here would place them in a repository and
in this template's history. A renderer that is handed one should refuse it.

| Token | Meaning |
|---|---|
| `<TENANT_ID>` | the tenant's identifier, lower-case |
| `<GIT_ORG>` | the tenant's git organisation |
| `<GITOPS_REPO_URL>` | the rendered repository's own URL |
| `<PLATFORM_REPO_URL>` | where the platform bundle is sourced from |
| `<BUNDLE_VERSION>` | the bundle version this cluster runs |
| `<CLUSTER_NAME>` | the cluster this directory describes |
| `<ENVIRONMENT>` | a workload cluster's environment, and the Day-0 bootstrap workflow's `environment` input. NOT the hub's: the management cluster declares none (ADR-111 Revision 7) |
| `<CLOUD_PROVIDER>` | provider the cluster is provisioned on |
| `<CLOUD_REGION>` | region the cluster runs in |
| `<DOMAIN_NAME>` | the tenant's base domain |
| `<DOMAIN>` | the DNS zone this box publishes under, e.g. `acme.example` |
| `<SUBDOMAIN>` | the label it sits at within that zone, or empty for the apex (ADR-051 amendment 2026-09-18) |
| `<HUB_DOMAIN>` | the two joined: `<subdomain>.<domain>`, or `<domain>` when no subdomain is declared |

## Tokens you fill in yourself

The tokens above are substituted when the repository is rendered. These are not:
they are in the templates the repository keeps, and they are replaced when you
copy a template to declare an application. They were undocumented, and the two
templates spelled the same thing three ways -- an application was `<APP_ID>`,
`<APP>` and `<APPLICATION>`, and its owner was both `<TENANT_ID>` and
`<FLEET_ID>`, a leftover from before ADR-088 separated the tenant from the
product it runs.

| Token | Meaning |
|---|---|
| `<APP_ID>` | the application: the product, and its isolation unit |
| `<WORKLOAD>` | one deployable inside that application, and its chart's name |
| `<TARGET_CLUSTER>` | the managed cluster the application runs on, by name |
| `<OWNER_EMAIL>` | who is paged for this application |
| `<KEY>` | a secret's name, in the example commands only |
