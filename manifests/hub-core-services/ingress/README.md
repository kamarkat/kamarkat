# Ingress, DNS, and TLS Configuration

## Overview

This directory contains Ingress configurations for Demo 1 OAuth2 PKCE flow.

## Components

### Ingress Routes

- **api.nutgraf.in** → AgentGateway (api-gateway namespace)
- **auth.nutgraf.in** → auth-proxy (identity-services namespace)
- **console.nutgraf.in** → Zitadel console (platform-identity namespace)

### TLS Configuration

Two modes supported:

1. **Production (Let's Encrypt)**
   - Uses cert-manager with Let's Encrypt ACME
   - Automatic certificate issuance and renewal
   - Requires public DNS and accessible endpoints

2. **Local Development (mkcert)**
   - Self-signed certificates trusted by local OS
   - No external dependencies
   - Perfect for local testing

## Prerequisites

### Production
- cert-manager installed
- nginx-ingress-controller installed
- Public DNS records pointing to cluster

### Local Development
- nginx-ingress-controller installed
- mkcert installed: `brew install mkcert`

## Local Development Setup

### 1. Install mkcert and generate certificates

```bash
./generate-local-certs.sh
```

This creates locally-trusted certificates in `./certs/`

### 2. Create Kubernetes TLS secrets

```bash
kubectl create secret tls api-kamarkat-tls \
  --cert=certs/api.nutgraf.in.crt \
  --key=certs/api.nutgraf.in.key \
  -n api-gateway

kubectl create secret tls auth-kamarkat-tls \
  --cert=certs/auth.nutgraf.in.crt \
  --key=certs/auth.nutgraf.in.key \
  -n identity-services

kubectl create secret tls console-kamarkat-tls \
  --cert=certs/console.nutgraf.in.crt \
  --key=certs/console.nutgraf.in.key \
  -n ory-system
```

### 3. Add DNS entries to /etc/hosts

```bash
# Get your ingress controller IP
kubectl get svc -n ingress-nginx ingress-nginx-controller

# Add entries (replace 127.0.0.1 with actual IP if needed)
./setup-local-dns.sh 127.0.0.1
```

### 4. Deploy Ingress resources

```bash
kubectl apply -k .
```

### 5. Verify

```bash
# Check Ingress resources
kubectl get ingress -A

# Test endpoints
curl https://api.nutgraf.in/health
curl https://auth.nutgraf.in/health/ready
curl https://console.nutgraf.in
```

## Production Deployment

### 1. Configure DNS

Create A records pointing to your cluster's load balancer:
```
api.nutgraf.in      → <LOAD_BALANCER_IP>
auth.nutgraf.in     → <LOAD_BALANCER_IP>
console.nutgraf.in  → <LOAD_BALANCER_IP>
```

### 2. Update email in ClusterIssuer

Edit `cluster-issuer.yaml` and set your email:
```yaml
spec:
  acme:
    email: your-email@example.com
```

### 3. Deploy

```bash
kubectl apply -k .
```

### 4. Verify certificates

```bash
kubectl get certificate -A
kubectl describe certificate api-kamarkat-dev -n api-gateway
```

## Troubleshooting

### Certificate not issued

```bash
# Check cert-manager logs
kubectl logs -n cert-manager deploy/cert-manager

# Check certificate status
kubectl describe certificate <name> -n <namespace>

# Check challenge status
kubectl get challenge -A
```

### Ingress not working

```bash
# Check ingress controller logs
kubectl logs -n ingress-nginx deploy/ingress-nginx-controller

# Verify ingress resources
kubectl describe ingress <name> -n <namespace>

# Check service endpoints
kubectl get endpoints -n <namespace>
```

### DNS not resolving

```bash
# Verify /etc/hosts entries
cat /etc/hosts | grep kamarkat

# Test DNS resolution
nslookup api.nutgraf.in
```

### TLS errors in browser

For local development:
- Ensure mkcert CA is installed: `mkcert -install`
- Restart browser after installing CA
- Check certificate: `openssl s_client -connect api.nutgraf.in:443`

## Cleanup

### Remove local DNS entries

```bash
sudo sed -i '' '/Kamarkat Demo 1/,+3d' /etc/hosts
```

### Remove certificates

```bash
rm -rf certs/
kubectl delete secret api-kamarkat-tls -n api-gateway
kubectl delete secret auth-kamarkat-tls -n identity-services
kubectl delete secret console-kamarkat-tls -n ory-system
```

### Remove Ingress resources

```bash
kubectl delete -k .
```
