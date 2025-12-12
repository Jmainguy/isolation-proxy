# Isolation Proxy Operator Helm Chart

A Kubernetes operator that provides TCP proxy functionality with warm pool management and per-connection pod isolation for game servers and other stateful applications.

## Features

- **TCP Proxy with Warm Pool**: Maintains a pool of ready pods for instant connection handling
- **Per-Connection Pod Assignment**: Each connection gets a dedicated pod (1:1 isolation)
- **Automatic Scaling**: Scales up on demand and scales down to maintain base pool + in-use pods
- **Multi-Namespace Support**: Deploy DedicatedServices in any namespace with isolated infrastructure
- **Connection Isolation**: IsolationPod CRD tracks pod usage to prevent reuse
- **Network Security**: NetworkPolicy blocks outbound internet from target pods
- **Configurable Service Types**: Support for ClusterIP, LoadBalancer, and NodePort

## Installation

### Prerequisites

- Kubernetes 1.19+
- Helm 3.0+

### Install the Chart

```bash
# Install the operator
helm install isolation-proxy-operator ./helm/isolation-proxy-operator \
  --namespace isolation-proxy-system \
  --create-namespace

# Verify installation
kubectl get deployment -n isolation-proxy-system
kubectl get crd dedicatedservices.soh.re isolationpods.soh.re
```

## Configuration

The following table lists the configurable parameters and their default values:

| Parameter | Description | Default |
|-----------|-------------|---------|
| `replicaCount` | Number of operator replicas | `1` |
| `image.repository` | Operator image repository | `zot.soh.re/isolation-proxy` |
| `image.pullPolicy` | Image pull policy | `Always` |
| `image.tag` | Image tag | `latest` |
| `serviceAccount.create` | Create service account | `true` |
| `resources.limits.cpu` | CPU limit | `500m` |
| `resources.limits.memory` | Memory limit | `256Mi` |
| `resources.requests.cpu` | CPU request | `100m` |
| `resources.requests.memory` | Memory request | `64Mi` |
| `metrics.enabled` | Enable metrics endpoint | `true` |
| `metrics.port` | Metrics port | `8080` |
| `healthProbe.enabled` | Enable health probes | `true` |
| `healthProbe.port` | Health probe port | `8081` |
| `leaderElection.enabled` | Enable leader election | `false` |

### Example Custom Values

```yaml
# values-custom.yaml
image:
  repository: zot.soh.re/isolation-proxy
  tag: "v1.0.0"

resources:
  limits:
    cpu: 1000m
    memory: 512Mi
  requests:
    cpu: 200m
    memory: 128Mi

leaderElection:
  enabled: true
```

Install with custom values:

```bash
helm install isolation-proxy-operator ./helm/isolation-proxy-operator \
  --namespace isolation-proxy-system \
  --create-namespace \
  -f values-custom.yaml
```

## Usage

### Create a DedicatedService

```yaml
apiVersion: soh.re/v1alpha1
kind: DedicatedService
metadata:
  name: my-game-server
  namespace: default
spec:
  image: my-registry.io/game-server:latest
  port: 8080
  replicas: 5  # Base warm pool size
  resources:
    requests:
      memory: "128Mi"
      cpu: "100m"
    limits:
      memory: "256Mi"
      cpu: "200m"
  service:
    type: LoadBalancer
    port: 9090
    annotations:
      service.beta.kubernetes.io/aws-load-balancer-type: "nlb"
```

### Check Status

```bash
# Use shorthand commands
kubectl get dedi -A
kubectl get ipods -A

# Or full names
kubectl get dedicatedservices.soh.re -A
kubectl get isolationpods.soh.re -A
```

Output:
```
NAMESPACE   NAME             IMAGE                      TOTAL   AVAILABLE   AGE
default     my-game-server   myregistry.io/game:latest  5       5           2m
```

## How It Works

1. **Warm Pool**: Operator maintains a pool of ready pods equal to `spec.replicas`
2. **Connection Handling**: Each incoming TCP connection gets assigned a dedicated pod
3. **IsolationPod Tracking**: Each pod gets an IsolationPod resource to track its usage status
4. **Scaling**: 
   - Scales up when a pod is assigned to a connection
   - Maintains warm pool at base replica count
   - Scales down to (in-use + base replicas) after connections close
5. **Pod Lifecycle**: Pods are deleted after their connection closes (never reused)
6. **Network Security**: NetworkPolicy blocks outbound internet while allowing proxy connections

## Uninstallation

```bash
# Delete all DedicatedServices first
kubectl delete dedi --all -A

# Uninstall the operator
helm uninstall isolation-proxy-operator --namespace isolation-proxy-system

# Optionally delete the namespace
kubectl delete namespace isolation-proxy-system
```

## Troubleshooting

### Check operator logs
```bash
kubectl logs -n isolation-proxy-system deployment/isolation-proxy-operator-controller-manager
```

### Check proxy logs
```bash
kubectl logs -n <namespace> deployment/<service-name>-proxy
```

### Check IsolationPod status
```bash
kubectl get ipods -n <namespace>
kubectl describe ipod -n <namespace> <ipod-name>
```

### Common Issues

1. **Pods not scaling**: Check RBAC permissions and operator logs
2. **Connection failures**: Verify service type and network policies
3. **High pod count**: Check if pool reaper is running and cleaning up idle pods
4. **Outbound connections blocked**: This is expected - NetworkPolicy blocks internet egress

## License

See the [LICENSE](../../LICENSE) file for details.
