---
inclusion: always
purpose: K8 Product development workflow
scope: Developement, testing guide
---

# K8 development guide

- During debugging, never use sleep commands more than 30 seconds.
- Provide the idiomatic enterprise grade widely adopted proven solutions to all the gaps respecting the platform principles.


### **Avoid works on my machine trap** of Kubernetes development:

**The Danger of Manual `kubectl apply`**

If the team continues to use `kubectl apply -f manifests/...` during development, you will face catastrophic failures when you move to production. Here is why:

*   **It masks dependency failures (Sync Waves):** In your `design.md` (Section 6.2), you explicitly defined ArgoCD `sync-waves` (e.g., Database Extensions must happen *before* NATS, which happens *before* AgentRegistry). If a developer manually applies the AgentRegistry YAML before the Database YAML, it might work locally because they "just figured it out." In production, ArgoCD will try to apply them, fail due to missing dependencies, and the deployment will crash.
*   **Configuration Drift:** Manual applies skip the ArgoCD validation hooks. A developer might apply a malformed YAML that Kubernetes accepts, but ArgoCD's strict sync policy will reject.
*   **It breaks the `open-sbt` abstraction:** The entire point of the platform is that **Git is the single source of truth**. If developers bypass Git to provision infrastructure, they aren't building a GitOps platform; they are just writing YAML scripts.

### Bash script for validation

*   **The Good:** The bash script you provided (`script.md`) is actually **excellent for validation**. It uses `kubectl exec` and `psql` to query the database and assert that the RLS policies and `pg_notify` triggers exist. Writing automated assertions like this is highly encouraged for Day 0 testing.
*   **The Bad:** Bash scripts should **ONLY be used for Read-Only assertions (Testing)**. They must **NEVER** be used to mutate state or apply infrastructure (no `kubectl apply`, `helm install`, etc.).

### The "Day 1 GitOps" Workflow

It is a mandate that **developer test changes exactly as they would happen in production from Day 1.** 

Here is the developer workflow you should enforce:

**Step 1: Local Cluster + ArgoCD Bootstrap (Done once per developer)**
Every developer should have the remote dev cluster access available from their machine. They run exactly *one* manual command: 

Check ArgoCD is already installed and pointed it to their current branch.

**Step 2: The Development Loop (GitOps Driven)**
1. The developer writes or modifies a manifest (e.g., adding a secret or XRD).
2. The developer **commits and pushes** the change to their feature branch.
3. ArgoCD detects the change and syncs it to the dev cluster. *(This proves the sync-waves and ArgoCD logic actually work).*
4. The developer runs their Bash Validation Script (`script.md`) to verify the end state is correct.

If a developer's feature works via `kubectl apply` but fails via Git commit → ArgoCD sync, **the feature is broken** and should fail the pull request.

### Actionable Guidance for Your Team
I recommend posting the following guidelines for development activities to immediately realign them with the specs:

**Team Update: Enforcing GitOps-First Development**

As we implement the Platform Core Services, we must adhere strictly to the `open-sbt` GitOps constraints. Moving forward:

1. **No Manual Applies:** The use of `kubectl apply -f` or `helm install` is strictly forbidden for deploying platform services, even in local development. 
2. **Test Like Production:** All infrastructure changes must be committed to your Git feature branch and synced to your dev cluster via **ArgoCD**. If it cannot be deployed by ArgoCD via our defined `sync-waves`, it is not production-ready.
3. **Validation Scripts:** The bash validation scripts (like checking PostgreSQL RLS policies) are excellent! Keep writing them. However, they must remain **100% read-only assertions**. They should test the state of the cluster *after* ArgoCD has finished syncing.
4. **Why?** We must catch dependency issues, RBAC failures, and sync-wave ordering bugs *now*, not on deployment day.

### Summary and Intent

Do not allow developer to work via isolated manual commands. Nip this in the bud now. By forcing them to test through ArgoCD during development, development might feel slightly slower today, but it guarantees that when you merge to `main`, your Day 0 platform will deploy flawlessly.

You are not allowed to violate GitOps principles by manually applying or creating directly for temporary fixes. Fix the issue permanantly. Do not create manually anything just to unblock progress. Everything must be via GitOps.

**Pro tip**: Developers can use manual forced sync kubectl patch to test their changes faster in right way instead of waiting for argocd auto-sync.

Example:
kubectl patch application platform-database -n argocd --type merge -p '{"spec":{"source":{"targetRevision":"272a135"}}}'

kubectl patch application platform-infisical-prerequisites -n argocd --type merge -p '{"operation":{"initiatedBy":{"username":"admin"},"sync":{"syncStrategy":{"hook":{},"apply":{"force":true}}}}}'

kubectl patch application platform-database -n argocd --type merge -p '{"spec":{"syncPolicy":{"automated":null}}}'

export KUBECONFIG=${KAMARKAT_HOME:-$HOME/.kamarkat}/run/kubeconfig/hub.kubeconfig && kubectl get pods
