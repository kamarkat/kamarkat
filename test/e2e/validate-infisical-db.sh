#!/bin/bash
set -euo pipefail

# Validation script for Infisical database and user setup
# This is a READ-ONLY script that verifies the state after GitOps deployment

echo "=== Infisical Database Validation ==="

# Check if infisical database exists
echo "1. Checking if infisical database exists..."
DB_EXISTS=$(kubectl exec infisical-db-1 -n platform-data -- psql -U postgres -t -c "SELECT 1 FROM pg_database WHERE datname='infisical';" 2>/dev/null | tr -d '[:space:]')

if [ "$DB_EXISTS" = "1" ]; then
    echo "✓ infisical database exists"
else
    echo "✗ infisical database does NOT exist"
    exit 1
fi

# Check if infisical role exists
echo "2. Checking if infisical role exists..."
ROLE_EXISTS=$(kubectl exec infisical-db-1 -n platform-data -- psql -U postgres -t -c "SELECT 1 FROM pg_roles WHERE rolname='infisical';" 2>/dev/null | tr -d '[:space:]')

if [ "$ROLE_EXISTS" = "1" ]; then
    echo "✓ infisical role exists"
else
    echo "✗ infisical role does NOT exist"
    exit 1
fi

# Check if infisical role has privileges on infisical database
echo "3. Checking if infisical role has privileges..."
HAS_PRIVS=$(kubectl exec infisical-db-1 -n platform-data -- psql -U postgres -d infisical -t -c "SELECT has_database_privilege('infisical', 'infisical', 'CREATE');" 2>/dev/null | tr -d '[:space:]')

if [ "$HAS_PRIVS" = "t" ]; then
    echo "✓ infisical role has CREATE privilege on infisical database"
else
    echo "✗ infisical role does NOT have CREATE privilege"
    exit 1
fi

# Check if infisical-db-credentials secret exists
echo "4. Checking if infisical-db-credentials secret exists..."
if kubectl get secret infisical-db-credentials -n hub-platform-data &>/dev/null; then
    echo "✓ infisical-db-credentials secret exists"
else
    echo "✗ infisical-db-credentials secret does NOT exist"
    exit 1
fi

echo ""
echo "=== All Validations Passed ==="
