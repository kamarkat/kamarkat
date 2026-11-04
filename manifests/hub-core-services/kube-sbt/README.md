# kube-sbt Deployment

## Overview
kube-sbt is the REST API server for the kube-sbt metering and billing system. It provides HTTP endpoints for:
- User management (via the Zitadel identity provider)
- Usage queries (tenant and user-scoped)
- Entitlement checking
- Subscription management
- Invoice operations
- Read-only catalog access (meters, features, plans)

## Architecture
- **Namespace**: `platform-billing`
- **Replicas**: 2 (HA deployment)
- **Node Selector**: Worker nodes only
- **Service Mesh**: Istio sidecar injection enabled

## Dependencies
- **OpenMeter API**: `openmeter-api.platform-billing.svc.cluster.local`
- **Zitadel**: `id.dev.nutgraf.in` (via gateway)
- **Redis**: `redis.platform-billing.svc.cluster.local:6379`

## Deployment

### Prerequisites
1. OpenMeter deployed and healthy
2. Zitadel (identity provider) deployed
4. Redis deployed

### Build Docker Image
```bash
# Build the image
docker build -t ghcr.io/kamarkat/kamarkat/kube-sbt:latest -f cmd/kube-sbt/Dockerfile .

# Push to registry
docker push ghcr.io/kamarkat/kamarkat/kube-sbt:latest
```

### Deploy via ArgoCD
```bash
# Apply ArgoCD Application
kubectl apply -f manifests/argocd/apps/kube-sbt.yaml

# Wait for sync
argocd app sync kube-sbt
argocd app wait kube-sbt
```

### Deploy via kubectl (for testing)
```bash
# Apply manifests directly
kubectl apply -k manifests/hub-core-services/kube-sbt/

# Check deployment status
kubectl get pods -n platform-billing -l app=kube-sbt
kubectl logs -n platform-billing -l app=kube-sbt --tail=100
```

## Testing

### Health Check
```bash
kubectl port-forward -n platform-billing svc/kube-sbt 8080:80
curl http://localhost:8080/health
```

### API Endpoints
All API endpoints require JWT authentication except `/health`.

Example authenticated request:
```bash
curl -H "Authorization: Bearer <JWT_TOKEN>" \
  http://localhost:8080/api/v1/tenants/tenant-123/usage
```

## Network Policy
The deployment includes a NetworkPolicy that:
- **Ingress**: Allows traffic only from AgentGateway
- **Egress**: Allows traffic to:
  - DNS (kube-dns)
  - OpenMeter API
  - Zitadel
  - Redis

## Monitoring
- **Liveness Probe**: `/health` endpoint (10s interval)
- **Readiness Probe**: `/health` endpoint (5s interval)
- **Metrics**: Exposed via Istio sidecar

## Troubleshooting

### Pod not starting
```bash
# Check pod events
kubectl describe pod -n platform-billing -l app=kube-sbt

# Check logs
kubectl logs -n platform-billing -l app=kube-sbt --tail=100
```

### Connection issues
```bash
# Test OpenMeter connectivity
kubectl exec -n platform-billing -it <pod-name> -- wget -O- http://openmeter-api.platform-billing.svc.cluster.local/health

# Test Zitadel connectivity
kubectl exec -n platform-billing -it <pod-name> -- wget -O- https://id.dev.nutgraf.in/debug/endpoints
```

### Network Policy issues
```bash
# Check network policy
kubectl get networkpolicy -n platform-billing kube-sbt -o yaml

# Temporarily disable for testing
kubectl delete networkpolicy -n platform-billing kube-sbt
```

## Configuration

### Environment Variables
- `PORT`: HTTP server port (default: 8080)
- `OPENMETER_URL`: OpenMeter API URL
- `AUTH_PROVIDER`: identity provider to use (default and only accepted value: `zitadel`)
- `OIDC_ISSUER_URL`: Zitadel public issuer URL
- `REDIS_URL`: Redis server URL

### Resource Limits
- **Requests**: 100m CPU, 128Mi memory
- **Limits**: 500m CPU, 512Mi memory

## Security
- mTLS enabled for service-to-service communication
- Network policies enforce zero-trust networking
- JWT validation for all API endpoints
- RBAC enforcement for admin endpoints
