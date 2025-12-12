package proxy

import (
	"context"
	"fmt"
	"io"
	"net"
	"strings"
	"sync"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"

	"github.com/go-logr/logr"
	sohv1alpha1 "github.com/jmainguy/isolation-proxy/api/v1alpha1"
)

const (
	// ReaperInterval is how often to clean up old/unused pods
	ReaperInterval = 10 * time.Second
)

// ProxyServer handles TCP connections and forwards them to pods
type ProxyServer struct {
	client      client.Client
	mu          sync.RWMutex
	podCache    map[string]*PodInfo
	namespace   string
	serviceName string // Specific DedicatedService to proxy for
	ctx         context.Context
}

// PodInfo stores information about a pod
type PodInfo struct {
	Name        string
	IP          string
	Port        int32
	LastUsed    time.Time
	ServiceName string
	InUse       bool
}

// NewProxyServer creates a new TCP proxy server
func NewProxyServer(client client.Client, namespace string, serviceName string) *ProxyServer {
	return &ProxyServer{
		client:      client,
		podCache:    make(map[string]*PodInfo),
		namespace:   namespace,
		serviceName: serviceName,
		ctx:         context.Background(),
	}
}

// HandleConnection handles incoming TCP connections
func (p *ProxyServer) HandleConnection(conn net.Conn) {
	logger := log.FromContext(p.ctx).WithValues("remote", conn.RemoteAddr())
	defer conn.Close()

	logger.Info("Incoming TCP connection")

	// Get a dedicated pod for this connection
	podInfo, err := p.getOrCreateDedicatedPod(p.ctx)
	if err != nil {
		logger.Error(err, "Failed to get or create dedicated pod")
		return
	}

	logger.Info("Assigned pod to connection", "pod", podInfo.Name, "service", podInfo.ServiceName)

	// Connect to the backend pod
	backendAddr := net.JoinHostPort(podInfo.IP, fmt.Sprintf("%d", podInfo.Port))
	logger.Info("Connecting to backend", "addr", backendAddr)

	backendConn, err := net.Dial("tcp", backendAddr)
	if err != nil {
		logger.Error(err, "Failed to connect to backend pod")
		// Mark pod as not in use since connection failed
		p.mu.Lock()
		if pod, exists := p.podCache[podInfo.Name]; exists {
			pod.InUse = false
		}
		p.mu.Unlock()
		return
	}
	defer backendConn.Close()

	// Update last used time
	p.mu.Lock()
	podInfo.LastUsed = time.Now()
	podInfo.InUse = true
	p.mu.Unlock()

	// Proxy traffic between client and backend using raw TCP
	errChan := make(chan error, 2)

	// Client -> Backend
	go func() {
		_, err := io.Copy(backendConn, conn)
		errChan <- err
	}()

	// Backend -> Client
	go func() {
		_, err := io.Copy(conn, backendConn)
		errChan <- err
	}()

	// Wait for either direction to close
	err = <-errChan
	if err != nil && err != io.EOF {
		logger.Error(err, "TCP proxy error")
	}

	logger.Info("Connection closed")

	// Delete the pod after connection closes
	go p.deletePod(p.ctx, podInfo.Name, podInfo.ServiceName)
}

// getOrCreateDedicatedPod gets an unused pod or scales up to create a new one
func (p *ProxyServer) getOrCreateDedicatedPod(ctx context.Context) (*PodInfo, error) {
	logger := log.FromContext(ctx)

	var targetService string

	// If a specific service name is configured, use it
	if p.serviceName != "" {
		targetService = p.serviceName
	} else {
		// Otherwise, list all DedicatedServices in the namespace and use the first one
		wssList := &sohv1alpha1.DedicatedServiceList{}
		err := p.client.List(ctx, wssList, &client.ListOptions{
			Namespace: p.namespace,
		})
		if err != nil {
			return nil, fmt.Errorf("failed to list DedicatedServices: %w", err)
		}

		if len(wssList.Items) == 0 {
			return nil, fmt.Errorf("no DedicatedServices found in namespace: %s", p.namespace)
		}

		targetService = wssList.Items[0].Name
	}

	// Try to find an unused pod first
	podInfo, err := p.findUnusedPod(ctx, targetService)
	if err == nil && podInfo != nil {
		// Mark this pod as used
		p.mu.Lock()
		p.podCache[podInfo.Name] = podInfo
		podInfo.InUse = true
		p.mu.Unlock()

		// Create/update IsolationPod resource
		if err := p.createOrUpdateIsolationPod(ctx, podInfo, true); err != nil {
			logger.Error(err, "Failed to create IsolationPod")
		}

		logger.Info("Using existing unused pod", "pod", podInfo.Name, "service", targetService)

		// Trigger pool maintenance to keep warm pool at minimum size
		go p.maintainPool(targetService)

		return podInfo, nil
	}

	// No unused pods, scale up the deployment
	logger.Info("No unused pods found, scaling up deployment", "service", targetService)
	err = p.scaleUpDeployment(ctx, targetService)
	if err != nil {
		return nil, fmt.Errorf("failed to scale up deployment: %w", err)
	}

	// Since we're maintaining a warm pool, a pod should be ready
	// Poll for a ready pod with a short timeout
	for i := 0; i < 10; i++ {
		time.Sleep(500 * time.Millisecond)
		podInfo, err = p.findUnusedPod(ctx, targetService)
		if err == nil && podInfo != nil {
			p.mu.Lock()
			p.podCache[podInfo.Name] = podInfo
			podInfo.InUse = true
			p.mu.Unlock()

			// Create/update IsolationPod resource
			if err := p.createOrUpdateIsolationPod(ctx, podInfo, true); err != nil {
				logger.Error(err, "Failed to create IsolationPod")
			}

			go p.maintainPool(targetService)

			return podInfo, nil
		}
	}

	return nil, fmt.Errorf("no ready pods available after scaling")
}

// findUnusedPod finds a ready pod that hasn't been assigned a connection yet
func (p *ProxyServer) findUnusedPod(ctx context.Context, serviceName string) (*PodInfo, error) {
	// Get the DedicatedService to find the port
	wss := &sohv1alpha1.DedicatedService{}
	err := p.client.Get(ctx, client.ObjectKey{
		Namespace: p.namespace,
		Name:      serviceName,
	}, wss)
	if err != nil {
		return nil, fmt.Errorf("failed to get DedicatedService: %w", err)
	}

	port := wss.Spec.Port
	if port == 0 {
		port = 8080
	}

	// List pods for this service
	podList := &corev1.PodList{}
	labelSelector := labels.SelectorFromSet(labels.Set{"app": serviceName})
	err = p.client.List(ctx, podList, &client.ListOptions{
		Namespace:     p.namespace,
		LabelSelector: labelSelector,
	})
	if err != nil {
		return nil, fmt.Errorf("failed to list pods: %w", err)
	}

	// Find a ready pod that isn't in use
	p.mu.RLock()
	defer p.mu.RUnlock()

	for _, pod := range podList.Items {
		if pod.Status.Phase == corev1.PodRunning {
			for _, condition := range pod.Status.Conditions {
				if condition.Type == corev1.PodReady && condition.Status == corev1.ConditionTrue {
					// Check if this pod is already in use
					if podInfo, exists := p.podCache[pod.Name]; exists && podInfo.InUse {
						continue
					}

					podInfo := &PodInfo{
						Name:        pod.Name,
						IP:          pod.Status.PodIP,
						Port:        port,
						LastUsed:    time.Now(),
						ServiceName: serviceName,
						InUse:       false,
					}

					// Create IsolationPod for this pod if it doesn't exist
					// This ensures all pods have an IsolationPod resource, not just in-use ones
					if err := p.createOrUpdateIsolationPod(ctx, podInfo, false); err != nil {
						logger := log.FromContext(ctx)
						logger.Error(err, "Failed to create IsolationPod for discovered pod", "pod", podInfo.Name)
					}

					return podInfo, nil
				}
			}
		}
	}

	return nil, fmt.Errorf("no unused ready pods found for service: %s", serviceName)
}

// scaleUpDedicatedService increases the replica count of a DedicatedService by 1
func (p *ProxyServer) scaleUpDeployment(ctx context.Context, serviceName string) error {
	logger := log.FromContext(ctx)

	// Retry on conflicts
	for i := 0; i < 3; i++ {
		// Get the DedicatedService
		wss := &sohv1alpha1.DedicatedService{}
		err := p.client.Get(ctx, client.ObjectKey{
			Namespace: p.namespace,
			Name:      serviceName,
		}, wss)
		if err != nil {
			return fmt.Errorf("failed to get DedicatedService: %w", err)
		}

		// Increase replica count by 1
		if wss.Spec.Replicas == 0 {
			wss.Spec.Replicas = 1
		} else {
			wss.Spec.Replicas++
		}

		// Update the DedicatedService
		err = p.client.Update(ctx, wss)
		if err == nil {
			logger.Info("Scaled up DedicatedService", "service", serviceName, "replicas", wss.Spec.Replicas)
			return nil
		}

		// If not a conflict error, return immediately
		if !strings.Contains(err.Error(), "Operation cannot be fulfilled") {
			return fmt.Errorf("failed to update DedicatedService: %w", err)
		}

		// Conflict - retry after a small delay
		if i < 2 {
			time.Sleep(100 * time.Millisecond)
		}
	}

	return fmt.Errorf("failed to update DedicatedService after 3 retries")
}

// ensureIsolationPodsForAllPods creates IsolationPod resources for all ready pods
// This ensures that all pods in the pool have IsolationPod tracking, not just in-use ones
// It also cleans up IsolationPods for pods that no longer exist
func (p *ProxyServer) ensureIsolationPodsForAllPods(ctx context.Context, serviceName string) {
	logger := log.FromContext(ctx).WithValues("service", serviceName)

	// Get the DedicatedService to find the port
	wss := &sohv1alpha1.DedicatedService{}
	err := p.client.Get(ctx, client.ObjectKey{
		Namespace: p.namespace,
		Name:      serviceName,
	}, wss)
	if err != nil {
		logger.Error(err, "Failed to get DedicatedService")
		return
	}

	port := wss.Spec.Port
	if port == 0 {
		port = 8080
	}

	// List all pods for this service
	podList := &corev1.PodList{}
	labelSelector := labels.SelectorFromSet(labels.Set{"app": serviceName})
	err = p.client.List(ctx, podList, &client.ListOptions{
		Namespace:     p.namespace,
		LabelSelector: labelSelector,
	})
	if err != nil {
		logger.Error(err, "Failed to list pods")
		return
	}

	// Build a map of existing pod names (excluding pods being deleted)
	podNames := make(map[string]bool)

	// For each ready pod, ensure it has an IsolationPod
	for _, pod := range podList.Items {
		// Skip pods that are being deleted
		if pod.DeletionTimestamp != nil {
			continue
		}

		podNames[pod.Name] = true

		if pod.Status.Phase == corev1.PodRunning {
			for _, condition := range pod.Status.Conditions {
				if condition.Type == corev1.PodReady && condition.Status == corev1.ConditionTrue {
					// Check if this pod is in use
					p.mu.RLock()
					podInfo, exists := p.podCache[pod.Name]
					inUse := exists && podInfo.InUse
					p.mu.RUnlock()

					// Create PodInfo if it doesn't exist
					if !exists {
						podInfo = &PodInfo{
							Name:        pod.Name,
							IP:          pod.Status.PodIP,
							Port:        port,
							LastUsed:    time.Now(),
							ServiceName: serviceName,
							InUse:       false,
						}
					}

					// Ensure IsolationPod exists for this pod
					if err := p.createOrUpdateIsolationPod(ctx, podInfo, inUse); err != nil {
						logger.Error(err, "Failed to ensure IsolationPod", "pod", pod.Name)
					}
				}
			}
		}
	}

	// Clean up IsolationPods for pods that no longer exist
	isolationPodList := &sohv1alpha1.IsolationPodList{}
	if err := p.client.List(ctx, isolationPodList, client.InNamespace(p.namespace), client.MatchingLabels(map[string]string{"soh.re/dedicatedservice": serviceName})); err == nil {
		logger.Info("Checking for orphaned IsolationPods", "totalIsolationPods", len(isolationPodList.Items), "totalPods", len(podNames))
		for _, ipod := range isolationPodList.Items {
			if !podNames[ipod.Spec.PodName] {
				// Pod no longer exists, delete the IsolationPod
				logger.Info("Deleting orphaned IsolationPod", "ipod", ipod.Name, "pod", ipod.Spec.PodName)
				if err := p.client.Delete(ctx, &ipod); err != nil {
					logger.Error(err, "Failed to delete orphaned IsolationPod", "ipod", ipod.Name)
				}
			}
		}
	} else {
		logger.Error(err, "Failed to list IsolationPods for cleanup")
	}
}

// maintainPool ensures we have at least the desired replica count ready pods available
func (p *ProxyServer) maintainPool(serviceName string) {
	ctx := p.ctx
	logger := log.FromContext(ctx).WithValues("service", serviceName)

	// Get the base pool size (original replicas value from user's spec)
	minPoolSize := p.getBasePoolSize(ctx, serviceName)
	if minPoolSize == 0 {
		logger.Info("Could not determine pool size, skipping maintenance")
		return
	}

	// Ensure all ready pods have IsolationPod resources
	p.ensureIsolationPodsForAllPods(ctx, serviceName)

	// Count unused pods
	unusedCount := p.countUnusedPods(serviceName)

	logger.Info("Pool maintenance check", "unused", unusedCount, "min", minPoolSize)

	// Scale up if we're below the minimum
	if unusedCount < int(minPoolSize) {
		needed := int(minPoolSize) - unusedCount
		logger.Info("Scaling up to maintain pool", "needed", needed)

		for i := 0; i < needed; i++ {
			err := p.scaleUpDeployment(ctx, serviceName)
			if err != nil {
				logger.Error(err, "Failed to scale up for pool maintenance")
				return
			}
		}
	}
}

// countUnusedPods counts how many unused pods exist for a service
func (p *ProxyServer) countUnusedPods(serviceName string) int {
	ctx := p.ctx

	// List pods for this service
	podList := &corev1.PodList{}
	labelSelector := labels.SelectorFromSet(labels.Set{"app": serviceName})
	err := p.client.List(ctx, podList, &client.ListOptions{
		Namespace:     p.namespace,
		LabelSelector: labelSelector,
	})
	if err != nil {
		return 0
	}

	p.mu.RLock()
	defer p.mu.RUnlock()

	count := 0
	for _, pod := range podList.Items {
		if pod.Status.Phase == corev1.PodRunning {
			for _, condition := range pod.Status.Conditions {
				if condition.Type == corev1.PodReady && condition.Status == corev1.ConditionTrue {
					// Check if this pod is in use
					if podInfo, exists := p.podCache[pod.Name]; !exists || !podInfo.InUse {
						count++
					}
				}
			}
		}
	}

	return count
}

// getBasePoolSize gets the base pool size from annotation (original replicas value)
func (p *ProxyServer) getBasePoolSize(ctx context.Context, serviceName string) int32 {
	wss := &sohv1alpha1.DedicatedService{}
	err := p.client.Get(ctx, client.ObjectKey{
		Namespace: p.namespace,
		Name:      serviceName,
	}, wss)
	if err != nil {
		return 0
	}

	// Check for base-pool-size annotation first
	if basePoolSizeStr, ok := wss.Annotations["soh.re/base-pool-size"]; ok {
		if basePoolSize, err := parseInt32(basePoolSizeStr); err == nil {
			return basePoolSize
		}
	}

	// If no annotation, use current spec.replicas and set it
	basePoolSize := wss.Spec.Replicas
	if basePoolSize <= 0 {
		basePoolSize = 1
	}

	// Store it in annotation for future reference
	if wss.Annotations == nil {
		wss.Annotations = make(map[string]string)
	}
	wss.Annotations["soh.re/base-pool-size"] = fmt.Sprintf("%d", basePoolSize)
	_ = p.client.Update(ctx, wss)

	return basePoolSize
}

func parseInt32(s string) (int32, error) {
	var i int32
	_, err := fmt.Sscanf(s, "%d", &i)
	return i, err
}

// deletePod deletes a pod and triggers pool maintenance
func (p *ProxyServer) deletePod(ctx context.Context, podName string, serviceName string) {
	logger := log.FromContext(ctx).WithValues("pod", podName, "service", serviceName)

	// Get pod info before removing from cache
	p.mu.RLock()
	podInfo, exists := p.podCache[podName]
	p.mu.RUnlock()

	// Remove from cache
	p.mu.Lock()
	delete(p.podCache, podName)
	p.mu.Unlock()

	// Delete IsolationPod resource if it exists
	if exists && podInfo != nil {
		if err := p.deleteIsolationPod(ctx, podInfo); err != nil {
			logger.Error(err, "Failed to delete IsolationPod resource")
		}
	}

	// Delete the pod from Kubernetes
	pod := &corev1.Pod{}
	err := p.client.Get(ctx, client.ObjectKey{
		Namespace: p.namespace,
		Name:      podName,
	}, pod)
	if err != nil {
		logger.Error(err, "Failed to get pod for deletion")
		return
	}

	err = p.client.Delete(ctx, pod)
	if err != nil {
		logger.Error(err, "Failed to delete pod")
		return
	}

	logger.Info("Deleted pod after connection closed")

	// Trigger pool maintenance
	go p.maintainPool(serviceName)

	// Scale down excess pods after a delay
	go func() {
		time.Sleep(5 * time.Second)
		p.scaleDownExcessPods(context.Background(), serviceName)
	}()
}

// createOrUpdateIsolationPod creates or updates an IsolationPod resource
func (p *ProxyServer) createOrUpdateIsolationPod(ctx context.Context, podInfo *PodInfo, inUse bool) error {
	logger := log.FromContext(ctx).WithValues("pod", podInfo.Name, "service", podInfo.ServiceName)

	ipod := &sohv1alpha1.IsolationPod{}
	ipodName := fmt.Sprintf("%s-%s", podInfo.ServiceName, podInfo.Name)
	err := p.client.Get(ctx, client.ObjectKey{
		Namespace: p.namespace,
		Name:      ipodName,
	}, ipod)

	now := metav1.Now()

	if err != nil {
		// Create new IsolationPod
		ipod = &sohv1alpha1.IsolationPod{
			ObjectMeta: metav1.ObjectMeta{
				Name:      ipodName,
				Namespace: p.namespace,
				Labels: map[string]string{
					"soh.re/dedicatedservice": podInfo.ServiceName,
					"soh.re/pod":              podInfo.Name,
				},
			},
			Spec: sohv1alpha1.IsolationPodSpec{
				ServiceName: podInfo.ServiceName,
				PodName:     podInfo.Name,
				PodIP:       podInfo.IP,
				Port:        podInfo.Port,
			},
			Status: sohv1alpha1.IsolationPodStatus{
				InUse: inUse,
				Phase: "Active",
			},
		}
		if inUse {
			ipod.Status.ConnectionStartTime = &now
		}
		ipod.Status.LastUsedTime = &now

		if err := p.client.Create(ctx, ipod); err != nil {
			logger.Error(err, "Failed to create IsolationPod")
			return err
		}
		logger.Info("Created IsolationPod", "inUse", inUse)
	} else {
		// Update existing IsolationPod
		ipod.Status.InUse = inUse
		if inUse && ipod.Status.ConnectionStartTime == nil {
			ipod.Status.ConnectionStartTime = &now
		} else if !inUse {
			ipod.Status.ConnectionStartTime = nil
		}
		ipod.Status.LastUsedTime = &now

		if err := p.client.Status().Update(ctx, ipod); err != nil {
			logger.Error(err, "Failed to update IsolationPod status")
			return err
		}
		logger.Info("Updated IsolationPod", "inUse", inUse)
	}

	return nil
}

// deleteIsolationPod removes an IsolationPod resource
func (p *ProxyServer) deleteIsolationPod(ctx context.Context, podInfo *PodInfo) error {
	logger := log.FromContext(ctx).WithValues("pod", podInfo.Name, "service", podInfo.ServiceName)

	ipodName := fmt.Sprintf("%s-%s", podInfo.ServiceName, podInfo.Name)
	ipod := &sohv1alpha1.IsolationPod{}
	err := p.client.Get(ctx, client.ObjectKey{
		Namespace: p.namespace,
		Name:      ipodName,
	}, ipod)
	if err != nil {
		// Already deleted, that's fine
		return nil
	}

	if err := p.client.Delete(ctx, ipod); err != nil {
		logger.Error(err, "Failed to delete IsolationPod")
		return err
	}
	logger.Info("Deleted IsolationPod")
	return nil
}

// scaleDownExcessPods scales down to maintain only in-use + base pool size total pods
func (p *ProxyServer) scaleDownExcessPods(ctx context.Context, serviceName string) {
	logger := log.FromContext(ctx).WithValues("service", serviceName)

	// Get the base pool size (original desired replicas)
	basePoolSize := p.getBasePoolSize(ctx, serviceName)
	if basePoolSize <= 0 {
		basePoolSize = 1
	}

	// Count in-use pods
	p.mu.RLock()
	inUseCount := 0
	for _, podInfo := range p.podCache {
		if podInfo.ServiceName == serviceName && podInfo.InUse {
			inUseCount++
		}
	}
	p.mu.RUnlock()

	// Target total should be in-use + base pool size
	targetTotal := int32(inUseCount) + basePoolSize

	// Get the DedicatedService to check current replicas
	wss := &sohv1alpha1.DedicatedService{}
	err := p.client.Get(ctx, client.ObjectKey{
		Namespace: p.namespace,
		Name:      serviceName,
	}, wss)
	if err != nil {
		logger.Error(err, "Failed to get DedicatedService for scale down")
		return
	}

	currentReplicas := wss.Spec.Replicas
	if currentReplicas <= targetTotal {
		// Already at or below target
		return
	}

	// Scale down to target
	logger.Info("Scaling down to target", "current", currentReplicas, "target", targetTotal, "inUse", inUseCount, "basePool", basePoolSize)

	// Update DedicatedService replicas
	for i := 0; i < 3; i++ {
		// Re-fetch to get latest version
		wss := &sohv1alpha1.DedicatedService{}
		err := p.client.Get(ctx, client.ObjectKey{
			Namespace: p.namespace,
			Name:      serviceName,
		}, wss)
		if err != nil {
			logger.Error(err, "Failed to get DedicatedService for scale down")
			return
		}

		wss.Spec.Replicas = targetTotal
		err = p.client.Update(ctx, wss)
		if err == nil {
			logger.Info("Scaled down DedicatedService", "service", serviceName, "replicas", targetTotal)
			return
		}

		// If not a conflict error, return immediately
		if !strings.Contains(err.Error(), "Operation cannot be fulfilled") {
			logger.Error(err, "Failed to scale down DedicatedService")
			return
		}

		// Conflict - retry after a small delay
		if i < 2 {
			time.Sleep(100 * time.Millisecond)
		}
	}

	logger.Error(nil, "Failed to scale down DedicatedService after 3 retries")
}

// poolReaper periodically cleans up old/unused pods
func (p *ProxyServer) poolReaper() {
	ticker := time.NewTicker(ReaperInterval)
	defer ticker.Stop()

	logger := log.FromContext(p.ctx).WithName("pool-reaper")
	logger.Info("Pool reaper started", "interval", ReaperInterval)

	for {
		select {
		case <-p.ctx.Done():
			logger.Info("Pool reaper stopped")
			return
		case <-ticker.C:
			logger.Info("Pool reaper running maintenance cycle")
			p.reapOldPods(logger)
			// Also ensure all pods have IsolationPod resources
			if p.serviceName != "" {
				p.ensureIsolationPodsForAllPods(p.ctx, p.serviceName)
			}
			logger.Info("Pool reaper maintenance cycle complete")
		}
	}
}

// reapOldPods removes pods that have been unused for too long or are in bad state
func (p *ProxyServer) reapOldPods(logger logr.Logger) {
	p.mu.Lock()
	defer p.mu.Unlock()

	now := time.Now()
	maxIdleTime := 5 * time.Minute

	for podName, podInfo := range p.podCache {
		// Skip pods currently in use
		if podInfo.InUse {
			continue
		}

		// Remove pods that have been idle for too long
		if now.Sub(podInfo.LastUsed) > maxIdleTime {
			logger.Info("Removing idle pod from cache", "pod", podName, "idle", now.Sub(podInfo.LastUsed))
			delete(p.podCache, podName)

			// Optionally, we could scale down the deployment here
			// but we'll let the pool maintenance handle the right size
		}
	}
}

// Start starts the TCP proxy server and background tasks
func (p *ProxyServer) Start(addr string) error {
	logger := log.FromContext(p.ctx)

	// Start the pool reaper
	logger.Info("Starting pool reaper goroutine")
	go p.poolReaper()

	// Start initial pool warm-up
	go func() {
		logger.Info("Starting pool warm-up in 2 seconds")
		time.Sleep(2 * time.Second) // Give controller time to create initial pods

		// If a specific service is configured, warm up only that service
		if p.serviceName != "" {
			logger.Info("Warming up pool for specific service", "service", p.serviceName)
			p.maintainPool(p.serviceName)
			return
		}

		// Otherwise warm up all services in the namespace
		wssList := &sohv1alpha1.DedicatedServiceList{}
		err := p.client.List(p.ctx, wssList, &client.ListOptions{
			Namespace: p.namespace,
		})
		if err != nil {
			logger.Error(err, "Failed to list DedicatedServices for warm-up")
			return
		}

		logger.Info("Found DedicatedServices for warm-up", "count", len(wssList.Items))
		if len(wssList.Items) > 0 {
			for _, wss := range wssList.Items {
				logger.Info("Warming up pool", "service", wss.Name)
				p.maintainPool(wss.Name)
			}
		}
	}()

	// Listen for TCP connections
	listener, err := net.Listen("tcp", addr)
	if err != nil {
		return fmt.Errorf("failed to listen on %s: %w", addr, err)
	}
	defer listener.Close()

	logger.Info("TCP proxy server listening", "addr", addr)

	for {
		conn, err := listener.Accept()
		if err != nil {
			logger.Error(err, "Failed to accept connection")
			continue
		}

		go p.HandleConnection(conn)
	}
}
