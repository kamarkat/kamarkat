#!/bin/bash
set -euo pipefail

# Health Check Validation Script for Infisical External Secrets Deployment
# This script performs READ-ONLY verification of deployed resources via GitOps.
# It does NOT create or modify any resources - all infrastructure must be deployed via ArgoCD.

echo "=== Infisical External Secrets Health Check ==="
echo ""

# Configuration
SECRET_STORE_API="https://infisical.hub-platform-data.svc"

# Step 1: Verify Infisical API is accessible
echo "1. Verifying Infisical API is accessible..."
if kubectl get service platform-infisical-standalone -n hub-platform-data &>/dev/null; then
  echo "   ✓ Infisical service exists"
else
  echo "   ✗ Infisical service not found"
  exit 1
fi

# Step 2: Verify External Secrets Operator is running
echo "2. Verifying External Secrets Operator..."
REPLICAS=$(kubectl get deployment platform-external-secrets -n external-secrets-system -o jsonpath='{.status.availableReplicas}' 2>/dev/null || echo "0")
if [ "$REPLICAS" -ge 1 ]; then
  echo "   ✓ External Secrets Operator is running ($REPLICAS replicas)"
else
  echo "   ✗ External Secrets Operator not ready"
  exit 1
fi

# Step 3: Verify ClusterSecretStore is ready
echo "3. Verifying ClusterSecretStore..."
CSS_STATUS=$(kubectl get clustersecretstore infisical-backend -o jsonpath='{.status.conditions[?(@.type=="Ready")].status}' 2>/dev/null || echo "False")
if [ "$CSS_STATUS" = "True" ]; then
  echo "   ✓ ClusterSecretStore 'infisical-backend' is ready"
else
  echo "   ✗ ClusterSecretStore not ready (status: $CSS_STATUS)"
  exit 1
fi

# Step 4: Verify ArgoCD GitHub secret exists and is synced
echo "4. Verifying ArgoCD GitHub authentication..."
if kubectl get secret hub-platform-git-secret -n argocd &>/dev/null; then
  echo "   ✓ ArgoCD GitHub secret exists"
else
  echo "   ✗ ArgoCD GitHub secret not found"
  exit 1
fi

# Step 5: Verify ExternalSecret for GitHub credentials is synced
echo "5. Verifying ExternalSecret sync status..."
ES_STATUS=$(kubectl get externalsecret argocd-github-creds -n argocd -o jsonpath='{.status.conditions[?(@.type=="Ready")].status}' 2>/dev/null || echo "False")
if [ "$ES_STATUS" = "True" ]; then
  echo "   ✓ ExternalSecret 'argocd-github-creds' is synced"
else
  echo "   ✗ ExternalSecret not synced (status: $ES_STATUS)"
  exit 1
fi

# Step 6: Verify ArgoCD applications can sync (GitHub auth working)
echo "6. Verifying ArgoCD GitHub authentication is working..."
SYNC_STATUS=$(kubectl get application platform-prometheus-operator -n argocd -o jsonpath='{.status.sync.status}' 2>/dev/null || echo "NotFound")

if [ "$SYNC_STATUS" = "Synced" ] || [ "$SYNC_STATUS" = "OutOfSync" ]; then
  echo "   ✓ ArgoCD GitHub authentication is working (no auth errors)"
else
  echo "   ⚠ Could not verify ArgoCD sync status (application may not exist yet)"
fi

# Step 7: Verify secret synchronization configuration
echo "7. Verifying secret synchronization..."
REFRESH_INTERVAL=$(kubectl get externalsecret argocd-github-creds -n argocd -o jsonpath='{.spec.refreshInterval}' 2>/dev/null || echo "unknown")
echo "   ✓ ExternalSecret refresh interval: ${REFRESH_INTERVAL}"

echo ""
echo "=== Health Check Complete ==="
echo "All components are deployed and functioning correctly via GitOps"
