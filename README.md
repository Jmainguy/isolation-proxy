# isolation-proxy - Kubernetes Operator for Connection Isolation

A Kubernetes operator that provides TCP proxy functionality with warm pool management and per-connection pod isolation.

## Overview

`isolation-proxy` is a Kubernetes operator that:
- Defines two Custom Resource Definitions: `DedicatedService` and `IsolationPod`
- Maintains a warm pool of ready pods for instant connection handling
- Routes each TCP connection to a dedicated pod (1:1 isolation)
- Implements NetworkPolicy to prevent outbound internet abuse
- Automatically scales pods based on connection demand
- Cleans up pods after connections close

## Architecture

The operator consists of three main components:

1. **Controller**: Manages `DedicatedService` resources, creating isolated infrastructure per service
2. **TCP Proxy**: Handles incoming connections and assigns each to a dedicated pod
3. **IsolationPod Tracker**: Tracks pod usage via `IsolationPod` CRD to prevent pod reuse

## Quick Start

### Installation via Helm

```bash
# Install the operator
helm install isolation-proxy-operator ./helm/isolation-proxy-operator \
  --namespace isolation-proxy-system \
  --create-namespace

# Verify installation
kubectl get deployment -n isolation-proxy-system
kubectl get crd dedicatedservices.soh.re isolationpods.soh.re
```

### Creating a DedicatedService

Create a `DedicatedService` resource to define your application:

```yaml
apiVersion: soh.re/v1alpha1
kind: DedicatedService
metadata:
  name: my-app
  namespace: default
spec:
  image: my-app-image:latest
  port: 8080
  replicas: 5  # Base warm pool size
  resources:
    requests:
      memory: "64Mi"
      cpu: "100m"
    limits:
      memory: "128Mi"
      cpu: "200m"
  service:
    type: LoadBalancer
    port: 9090
  env:
  - name: LOG_LEVEL
    value: "info"
```

Apply the resource:
```bash
kubectl apply -f my-app.yaml

# Check status
kubectl get dedi -A
kubectl get ipods -A
```

### Connecting to Your Service

The operator creates a proxy service for each `DedicatedService`. Connect to it using:

```bash
# Get the proxy service
kubectl get svc -n default

# For LoadBalancer type
kubectl get svc my-app-proxy -o jsonpath='{.status.loadBalancer.ingress[0].ip}'

# Connect via TCP
telnet <proxy-ip> 9090
```

## DedicatedService Spec

| Field | Type | Description | Default |
|-------|------|-------------|---------|
| `image` | string | Container image for the pods (required) | - |
| `port` | int32 | Port the container listens on | 8080 |
| `replicas` | int32 | Base warm pool size (minimum: 0) | 1 |
| `resources` | ResourceRequirements | Resource requests and limits | - |
| `env` | []EnvVar | Environment variables for the container | - |
| `service.type` | string | Service type (ClusterIP/LoadBalancer/NodePort) | ClusterIP |
| `service.port` | int32 | Proxy service port | 9090 |
| `service.annotations` | map[string]string | Service annotations | - |

## DedicatedService Status

| Field | Description |
|-------|-------------|
| `totalTargets` | Total number of target pods |
| `availableTargets` | Number of ready pods not in use |

## Development

### Building and Testing

```bash
# Run tests
make test

# Format code
make fmt

# Vet code
make vet

# Build Docker image
make docker-build

# Push to registry
make docker-push
```

### Deploying Changes

```bash
# Build and push
make docker-build && make docker-push

# Upgrade Helm release
make helm-upgrade
```

## How It Works

1. **Resource Creation**: When you create a `DedicatedService`, the controller creates:
   - A dedicated namespace for isolated infrastructure
   - A Deployment with the specified image
   - A proxy Deployment with RBAC (ServiceAccount, Role, RoleBinding)
   - A proxy Service (configurable type: ClusterIP/LoadBalancer/NodePort)
   - A NetworkPolicy blocking outbound internet from target pods
   - IsolationPod resources tracking each pod

2. **Warm Pool**: The operator maintains a pool of ready pods:
   - Pool size equals `spec.replicas` (configurable via `soh.re/base-pool-size` annotation)
   - Pool reaper runs every 10 seconds to ensure pods exist and clean idle pods
   - Pods are created ahead of time for instant assignment

3. **Connection Handling**: When a TCP connection arrives:
   - Proxy finds an unused pod from the warm pool
   - Creates/updates IsolationPod to mark it in-use
   - Establishes bidirectional TCP connection
   - Pod is deleted after connection closes (never reused)

4. **Scaling**: The system automatically scales:
   - Up: Creates new pods when connections consume the pool
   - Down: Removes pods after connections close, maintaining base pool + in-use
   - IsolationPod CRD tracks which pods are in-use

5. **Security**: NetworkPolicy prevents abuse:
   - Allows all ingress (proxy connections)
   - Allows DNS queries (UDP/TCP port 53 to kube-system)
   - Allows egress within same namespace
   - Blocks all other egress (internet)

## Project Structure

```
soh-k8s/
├── api/v1alpha1/                           # API type definitions
│   ├── dedicatedservice_types.go          # DedicatedService CRD
│   ├── isolationpod_types.go              # IsolationPod CRD
│   └── groupversion_info.go               # API group: soh.re
├── cmd/                                    # Main application
│   └── main.go
├── internal/
│   ├── controller/                        # Controller logic
│   │   └── dedicatedservice_controller.go
│   └── proxy/                             # TCP proxy server
│       └── proxy.go
├── helm/isolation-proxy-operator/         # Helm chart
│   ├── Chart.yaml
│   ├── values.yaml
│   ├── templates/
│   │   ├── crd.yaml                      # Both CRDs
│   │   ├── isolationpod-crd.yaml
│   │   ├── deployment.yaml
│   │   ├── clusterrole.yaml              # RBAC permissions
│   │   └── ...
│   └── README.md
├── example-dedicatedservice.yaml          # Example resource
├── Dockerfile                             # Container image
├── Makefile                               # Build targets
└── README.md
```

## Makefile Targets

| Target | Description |
|--------|-------------|
| `make build` | Build the manager binary |
| `make docker-build` | Build the Docker image |
| `make test` | Run tests |
| `make fmt` | Format code |
| `make vet` | Run go vet |
| `make tidy` | Run go mod tidy |
| `make helm-install` | Install Helm chart |
| `make helm-upgrade` | Upgrade Helm release |
| `make helm-uninstall` | Uninstall Helm release |
| `make deploy-example` | Deploy example DedicatedService |

## Configuration

### Operator Configuration (via Helm values.yaml)

| Parameter | Description | Default |
|-----------|-------------|---------|
| `image.repository` | Operator image | `zot.soh.re/isolation-proxy` |
| `image.tag` | Image tag | `latest` |
| `replicaCount` | Operator replicas | `1` |
| `resources.limits.cpu` | CPU limit | `500m` |
| `resources.limits.memory` | Memory limit | `256Mi` |
| `leaderElection.enabled` | Leader election | `false` |

### DedicatedService Annotations

| Annotation | Description | Default |
|------------|-------------|---------|
| `soh.re/base-pool-size` | Override base warm pool size | `spec.replicas` |

## Monitoring

The operator exposes the following endpoints:

- **Metrics**: `:8080/metrics` - Prometheus metrics
- **Health**: `:8081/healthz` - Liveness probe
- **Ready**: `:8081/readyz` - Readiness probe

## Troubleshooting

### Check operator logs
```bash
kubectl logs -n isolation-proxy-system deployment/isolation-proxy-operator-controller-manager
```

### Check DedicatedService status
```bash
kubectl get dedi -A
kubectl describe dedi my-app
```

### Check IsolationPod status
```bash
kubectl get ipods -A
kubectl get ipods -n my-app-namespace
```

### Check proxy logs
```bash
kubectl logs -n my-app-namespace deployment/my-app-proxy
```

### Check target pods
```bash
kubectl get pods -n my-app-namespace
kubectl describe pod -n my-app-namespace <pod-name>
```

### Check NetworkPolicy
```bash
kubectl get networkpolicy -n my-app-namespace
kubectl describe networkpolicy -n my-app-namespace
```

## Contributing

Contributions are welcome! Please feel free to submit a Pull Request.

## Features

- ✅ TCP proxy with warm pool management
- ✅ Per-connection pod isolation (1:1 mapping)
- ✅ Automatic scaling (up on demand, down to base pool)
- ✅ Multi-namespace support (isolated infrastructure per service)
- ✅ IsolationPod CRD for usage tracking
- ✅ NetworkPolicy for outbound internet blocking
- ✅ Configurable service types (ClusterIP/LoadBalancer/NodePort)
- ✅ RBAC isolation per DedicatedService
- ✅ Pool reaper for maintenance and cleanup
- ✅ Shorthand commands (dedi, ipod, ipods)

## Future Enhancements

- [ ] Metrics dashboard for connection tracking
- [ ] Custom eviction policies
- [ ] Pod health checks and automatic replacement
- [ ] Connection timeout configuration
- [ ] TLS/SSL support for proxy connections

## Contact

For questions or support, please open an issue on GitHub.
