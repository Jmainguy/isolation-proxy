package controller

import (
	"context"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/intstr"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/log"

	sohv1alpha1 "github.com/jmainguy/isolation-proxy/api/v1alpha1"
)

const (
	finalizerName = "soh.re/finalizer"
)

// DedicatedServiceReconciler reconciles a DedicatedService object
type DedicatedServiceReconciler struct {
	client.Client
	Scheme *runtime.Scheme
}

// +kubebuilder:rbac:groups=soh.re,resources=dedicatedservices,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=soh.re,resources=dedicatedservices/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=soh.re,resources=dedicatedservices/finalizers,verbs=update
// +kubebuilder:rbac:groups=soh.re,resources=isolationpods,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=soh.re,resources=isolationpods/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=apps,resources=deployments,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=soh.re,resources=isolationpods/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=apps,resources=deployments,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=core,resources=services,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=core,resources=pods,verbs=get;list;watch
// +kubebuilder:rbac:groups=core,resources=serviceaccounts,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=rbac.authorization.k8s.io,resources=roles,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=rbac.authorization.k8s.io,resources=rolebindings,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=networking.k8s.io,resources=networkpolicies,verbs=get;list;watch;create;update;patch;delete

func (r *DedicatedServiceReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	logger := log.FromContext(ctx)

	// Fetch the DedicatedService instance
	wss := &sohv1alpha1.DedicatedService{}
	err := r.Get(ctx, req.NamespacedName, wss)
	if err != nil {
		if errors.IsNotFound(err) {
			logger.Info("DedicatedService resource not found. Ignoring since object must be deleted")
			return ctrl.Result{}, nil
		}
		logger.Error(err, "Failed to get DedicatedService")
		return ctrl.Result{}, err
	}

	// Handle deletion
	if !wss.ObjectMeta.DeletionTimestamp.IsZero() {
		if controllerutil.ContainsFinalizer(wss, finalizerName) {
			// Cleanup logic here if needed
			controllerutil.RemoveFinalizer(wss, finalizerName)
			if err := r.Update(ctx, wss); err != nil {
				return ctrl.Result{}, err
			}
		}
		return ctrl.Result{}, nil
	}

	// Add finalizer if not present
	if !controllerutil.ContainsFinalizer(wss, finalizerName) {
		controllerutil.AddFinalizer(wss, finalizerName)
		if err := r.Update(ctx, wss); err != nil {
			return ctrl.Result{}, err
		}
	}

	// Reconcile ServiceAccount and RBAC for proxy
	if err := r.reconcileProxyRBAC(ctx, wss); err != nil {
		logger.Error(err, "Failed to reconcile Proxy RBAC")
		r.updateStatus(ctx, wss, false, "ProxyRBACFailed", err.Error())
		return ctrl.Result{}, err
	}

	// Reconcile target application Deployment
	if err := r.reconcileDeployment(ctx, wss); err != nil {
		logger.Error(err, "Failed to reconcile Deployment")
		r.updateStatus(ctx, wss, false, "DeploymentFailed", err.Error())
		return ctrl.Result{}, err
	}

	// Reconcile target application Service
	if err := r.reconcileService(ctx, wss); err != nil {
		logger.Error(err, "Failed to reconcile Service")
		r.updateStatus(ctx, wss, false, "ServiceFailed", err.Error())
		return ctrl.Result{}, err
	}

	// Reconcile Proxy Deployment
	if err := r.reconcileProxyDeployment(ctx, wss); err != nil {
		logger.Error(err, "Failed to reconcile Proxy Deployment")
		r.updateStatus(ctx, wss, false, "ProxyDeploymentFailed", err.Error())
		return ctrl.Result{}, err
	}

	// Reconcile Proxy Service
	if err := r.reconcileProxyService(ctx, wss); err != nil {
		logger.Error(err, "Failed to reconcile Proxy Service")
		r.updateStatus(ctx, wss, false, "ProxyServiceFailed", err.Error())
		return ctrl.Result{}, err
	}

	// Reconcile NetworkPolicy for target pods
	if err := r.reconcileNetworkPolicy(ctx, wss); err != nil {
		logger.Error(err, "Failed to reconcile NetworkPolicy")
		r.updateStatus(ctx, wss, false, "NetworkPolicyFailed", err.Error())
		return ctrl.Result{}, err
	}

	// Update status
	if err := r.updateStatus(ctx, wss, true, "Ready", "DedicatedService is ready"); err != nil {
		// Ignore conflict errors - they will be retried automatically
		if !errors.IsConflict(err) {
			logger.Error(err, "Failed to update status")
			return ctrl.Result{}, err
		}
		logger.V(1).Info("Status update conflict, will retry", "error", err)
	}

	return ctrl.Result{RequeueAfter: 30 * time.Second}, nil
}

func (r *DedicatedServiceReconciler) reconcileProxyRBAC(ctx context.Context, wss *sohv1alpha1.DedicatedService) error {
	serviceAccountName := wss.Name + "-proxy"

	// Create ServiceAccount
	sa := &corev1.ServiceAccount{}
	err := r.Get(ctx, types.NamespacedName{Name: serviceAccountName, Namespace: wss.Namespace}, sa)

	desiredSA := &corev1.ServiceAccount{
		ObjectMeta: metav1.ObjectMeta{
			Name:      serviceAccountName,
			Namespace: wss.Namespace,
			Labels: map[string]string{
				"app":                     wss.Name + "-proxy",
				"soh.re/dedicatedservice": wss.Name,
			},
		},
	}

	if err := controllerutil.SetControllerReference(wss, desiredSA, r.Scheme); err != nil {
		return err
	}

	if err != nil && errors.IsNotFound(err) {
		if err := r.Create(ctx, desiredSA); err != nil {
			return err
		}
	} else if err != nil {
		return err
	}

	// Create Role
	role := &rbacv1.Role{}
	roleName := wss.Name + "-proxy"
	err = r.Get(ctx, types.NamespacedName{Name: roleName, Namespace: wss.Namespace}, role)

	desiredRole := &rbacv1.Role{
		ObjectMeta: metav1.ObjectMeta{
			Name:      roleName,
			Namespace: wss.Namespace,
			Labels: map[string]string{
				"app":                     wss.Name + "-proxy",
				"soh.re/dedicatedservice": wss.Name,
			},
		},
		Rules: []rbacv1.PolicyRule{
			{
				APIGroups: []string{"soh.re"},
				Resources: []string{"dedicatedservices"},
				Verbs:     []string{"get", "list", "watch", "update", "patch"},
			},
			{
				APIGroups: []string{"soh.re"},
				Resources: []string{"dedicatedservices/status"},
				Verbs:     []string{"get", "update", "patch"},
			},
			{
				APIGroups: []string{"soh.re"},
				Resources: []string{"isolationpods"},
				Verbs:     []string{"get", "list", "watch", "create", "update", "patch", "delete"},
			},
			{
				APIGroups: []string{"soh.re"},
				Resources: []string{"dedicatedservices/status"},
				Verbs:     []string{"get", "update", "patch"},
			},
			{
				APIGroups: []string{"soh.re"},
				Resources: []string{"isolationpods"},
				Verbs:     []string{"get", "list", "watch", "create", "update", "patch", "delete"},
			},
			{
				APIGroups: []string{"soh.re"},
				Resources: []string{"isolationpods/status"},
				Verbs:     []string{"get", "update", "patch"},
			},
			{
				APIGroups: []string{""},
				Resources: []string{"pods"},
				Verbs:     []string{"get", "list", "watch", "delete"},
			},
		},
	}

	if err := controllerutil.SetControllerReference(wss, desiredRole, r.Scheme); err != nil {
		return err
	}

	if err != nil && errors.IsNotFound(err) {
		if err := r.Create(ctx, desiredRole); err != nil {
			return err
		}
	} else if err != nil {
		return err
	} else {
		role.Rules = desiredRole.Rules
		if err := r.Update(ctx, role); err != nil {
			return err
		}
	}

	// Create RoleBinding
	rb := &rbacv1.RoleBinding{}
	err = r.Get(ctx, types.NamespacedName{Name: roleName, Namespace: wss.Namespace}, rb)

	desiredRB := &rbacv1.RoleBinding{
		ObjectMeta: metav1.ObjectMeta{
			Name:      roleName,
			Namespace: wss.Namespace,
			Labels: map[string]string{
				"app":                     wss.Name + "-proxy",
				"soh.re/dedicatedservice": wss.Name,
			},
		},
		RoleRef: rbacv1.RoleRef{
			APIGroup: "rbac.authorization.k8s.io",
			Kind:     "Role",
			Name:     roleName,
		},
		Subjects: []rbacv1.Subject{
			{
				Kind:      "ServiceAccount",
				Name:      serviceAccountName,
				Namespace: wss.Namespace,
			},
		},
	}

	if err := controllerutil.SetControllerReference(wss, desiredRB, r.Scheme); err != nil {
		return err
	}

	if err != nil && errors.IsNotFound(err) {
		if err := r.Create(ctx, desiredRB); err != nil {
			return err
		}
	} else if err != nil {
		return err
	}

	return nil
}

func (r *DedicatedServiceReconciler) reconcileDeployment(ctx context.Context, wss *sohv1alpha1.DedicatedService) error {
	deployment := &appsv1.Deployment{}
	deploymentName := wss.Name
	err := r.Get(ctx, types.NamespacedName{Name: deploymentName, Namespace: wss.Namespace}, deployment)

	// Set default values
	replicas := wss.Spec.Replicas
	if replicas == 0 {
		replicas = 1
	}
	port := wss.Spec.Port
	if port == 0 {
		port = 8080
	}

	desiredDeployment := &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{
			Name:      deploymentName,
			Namespace: wss.Namespace,
			Labels: map[string]string{
				"app":                     wss.Name,
				"soh.re/dedicatedservice": wss.Name,
			},
		},
		Spec: appsv1.DeploymentSpec{
			Replicas: &replicas,
			Selector: &metav1.LabelSelector{
				MatchLabels: map[string]string{
					"app": wss.Name,
				},
			},
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{
					Labels: map[string]string{
						"app":                     wss.Name,
						"soh.re/dedicatedservice": wss.Name,
					},
				},
				Spec: corev1.PodSpec{
					Containers: []corev1.Container{
						{
							Name:      "websocket-app",
							Image:     wss.Spec.Image,
							Ports:     []corev1.ContainerPort{{ContainerPort: port}},
							Env:       wss.Spec.Env,
							Resources: wss.Spec.Resources,
						},
					},
				},
			},
		},
	}

	// Set DedicatedService instance as the owner
	if err := controllerutil.SetControllerReference(wss, desiredDeployment, r.Scheme); err != nil {
		return err
	}

	if err != nil && errors.IsNotFound(err) {
		// Create the deployment
		if err := r.Create(ctx, desiredDeployment); err != nil {
			return err
		}
		return nil
	} else if err != nil {
		return err
	}

	// Update the deployment if it exists using Patch to avoid conflicts
	patch := client.MergeFrom(deployment.DeepCopy())
	deployment.Spec = desiredDeployment.Spec
	if err := r.Patch(ctx, deployment, patch); err != nil {
		return err
	}

	return nil
}

func (r *DedicatedServiceReconciler) reconcileService(ctx context.Context, wss *sohv1alpha1.DedicatedService) error {
	service := &corev1.Service{}
	serviceName := wss.Name
	err := r.Get(ctx, types.NamespacedName{Name: serviceName, Namespace: wss.Namespace}, service)

	port := wss.Spec.Port
	if port == 0 {
		port = 8080
	}

	desiredService := &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{
			Name:      serviceName,
			Namespace: wss.Namespace,
			Labels: map[string]string{
				"app":                     wss.Name,
				"soh.re/dedicatedservice": wss.Name,
			},
		},
		Spec: corev1.ServiceSpec{
			Selector: map[string]string{
				"app": wss.Name,
			},
			Ports: []corev1.ServicePort{
				{
					Name:       "websocket",
					Protocol:   corev1.ProtocolTCP,
					Port:       port,
					TargetPort: intstr.FromInt(int(port)),
				},
			},
			Type: corev1.ServiceTypeClusterIP,
		},
	}

	// Set DedicatedService instance as the owner
	if err := controllerutil.SetControllerReference(wss, desiredService, r.Scheme); err != nil {
		return err
	}

	if err != nil && errors.IsNotFound(err) {
		// Create the service
		if err := r.Create(ctx, desiredService); err != nil {
			return err
		}
		return nil
	} else if err != nil {
		return err
	}

	// Update the service if it exists (preserve ClusterIP)
	desiredService.Spec.ClusterIP = service.Spec.ClusterIP
	desiredService.ResourceVersion = service.ResourceVersion
	if err := r.Update(ctx, desiredService); err != nil {
		return err
	}

	return nil
}

func (r *DedicatedServiceReconciler) updateStatus(ctx context.Context, wss *sohv1alpha1.DedicatedService, ready bool, reason, message string) error {
	// Fetch the latest version to avoid conflicts
	latest := &sohv1alpha1.DedicatedService{}
	if err := r.Get(ctx, types.NamespacedName{Name: wss.Name, Namespace: wss.Namespace}, latest); err != nil {
		return err
	}

	// Get current deployment status
	deployment := &appsv1.Deployment{}
	err := r.Get(ctx, types.NamespacedName{Name: latest.Name, Namespace: latest.Namespace}, deployment)
	if err == nil {
		latest.Status.AvailableReplicas = deployment.Status.AvailableReplicas
		latest.Status.TotalTargets = deployment.Status.Replicas
	}

	// Count target pods - get all pods with the label
	podList := &corev1.PodList{}
	labelSelector := map[string]string{"app": latest.Name}
	if err := r.List(ctx, podList, client.InNamespace(latest.Namespace), client.MatchingLabels(labelSelector)); err == nil {
		readyCount := int32(0)
		for _, pod := range podList.Items {
			if pod.Status.Phase == corev1.PodRunning {
				for _, condition := range pod.Status.Conditions {
					if condition.Type == corev1.PodReady && condition.Status == corev1.ConditionTrue {
						readyCount++
						break
					}
				}
			}
		}
		latest.Status.TotalTargets = int32(len(podList.Items))

		// Count in-use pods from IsolationPod resources
		inUseCount := int32(0)
		isolationPodList := &sohv1alpha1.IsolationPodList{}
		if err := r.List(ctx, isolationPodList, client.InNamespace(latest.Namespace), client.MatchingLabels(map[string]string{"soh.re/dedicatedservice": latest.Name})); err == nil {
			for _, ipod := range isolationPodList.Items {
				if ipod.Status.InUse {
					inUseCount++
				}
			}
		}

		latest.Status.AvailableTargets = readyCount - inUseCount
		if latest.Status.AvailableTargets < 0 {
			latest.Status.AvailableTargets = 0
		}
	}

	// Update conditions
	condition := metav1.Condition{
		Type:               "Ready",
		Status:             metav1.ConditionFalse,
		ObservedGeneration: latest.Generation,
		LastTransitionTime: metav1.Now(),
		Reason:             reason,
		Message:            message,
	}
	if ready {
		condition.Status = metav1.ConditionTrue
	}

	// Update or add condition
	found := false
	for i, c := range latest.Status.Conditions {
		if c.Type == "Ready" {
			latest.Status.Conditions[i] = condition
			found = true
			break
		}
	}
	if !found {
		latest.Status.Conditions = append(latest.Status.Conditions, condition)
	}

	return r.Status().Update(ctx, latest)
}

func (r *DedicatedServiceReconciler) reconcileProxyDeployment(ctx context.Context, wss *sohv1alpha1.DedicatedService) error {
	deployment := &appsv1.Deployment{}
	deploymentName := wss.Name + "-proxy"
	err := r.Get(ctx, types.NamespacedName{Name: deploymentName, Namespace: wss.Namespace}, deployment)

	replicas := int32(1)

	desiredDeployment := &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{
			Name:      deploymentName,
			Namespace: wss.Namespace,
			Labels: map[string]string{
				"app":                     wss.Name + "-proxy",
				"soh.re/dedicatedservice": wss.Name,
				"soh.re/component":        "proxy",
			},
		},
		Spec: appsv1.DeploymentSpec{
			Replicas: &replicas,
			Selector: &metav1.LabelSelector{
				MatchLabels: map[string]string{
					"app": wss.Name + "-proxy",
				},
			},
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{
					Labels: map[string]string{
						"app":                     wss.Name + "-proxy",
						"soh.re/dedicatedservice": wss.Name,
						"soh.re/component":        "proxy",
					},
				},
				Spec: corev1.PodSpec{
					ServiceAccountName: wss.Name + "-proxy",
					Containers: []corev1.Container{
						{
							Name:            "proxy",
							Image:           "zot.soh.re/isolation-proxy:latest",
							ImagePullPolicy: corev1.PullAlways,
							Command:         []string{"/manager"},
							Args: []string{
								"--proxy-only",
								"--namespace=" + wss.Namespace,
								"--service-name=" + wss.Name,
								"--proxy-bind-address=:9090",
							},
							Ports: []corev1.ContainerPort{
								{ContainerPort: 9090, Name: "proxy", Protocol: corev1.ProtocolTCP},
							},
							Resources: corev1.ResourceRequirements{
								Limits: corev1.ResourceList{
									corev1.ResourceCPU:    *resourceQuantity("500m"),
									corev1.ResourceMemory: *resourceQuantity("512Mi"),
								},
								Requests: corev1.ResourceList{
									corev1.ResourceCPU:    *resourceQuantity("100m"),
									corev1.ResourceMemory: *resourceQuantity("128Mi"),
								},
							},
						},
					},
				},
			},
		},
	}

	// Set DedicatedService instance as the owner
	if err := controllerutil.SetControllerReference(wss, desiredDeployment, r.Scheme); err != nil {
		return err
	}

	if err != nil && errors.IsNotFound(err) {
		// Create the deployment
		if err := r.Create(ctx, desiredDeployment); err != nil {
			return err
		}
		return nil
	} else if err != nil {
		return err
	}

	// Update the deployment if it exists using Patch to avoid conflicts
	patch := client.MergeFrom(deployment.DeepCopy())
	deployment.Spec = desiredDeployment.Spec
	if err := r.Patch(ctx, deployment, patch); err != nil {
		return err
	}

	return nil
}

func (r *DedicatedServiceReconciler) reconcileProxyService(ctx context.Context, wss *sohv1alpha1.DedicatedService) error {
	service := &corev1.Service{}
	serviceName := wss.Name + "-proxy"
	err := r.Get(ctx, types.NamespacedName{Name: serviceName, Namespace: wss.Namespace}, service)

	// Determine service type (default to ClusterIP)
	serviceType := corev1.ServiceTypeClusterIP
	if wss.Spec.Service != nil && wss.Spec.Service.Type != "" {
		serviceType = wss.Spec.Service.Type
	}

	// Determine service port (default to 9090)
	servicePort := int32(9090)
	if wss.Spec.Service != nil && wss.Spec.Service.Port != 0 {
		servicePort = wss.Spec.Service.Port
	}

	// Prepare annotations
	annotations := make(map[string]string)
	if wss.Spec.Service != nil && wss.Spec.Service.Annotations != nil {
		annotations = wss.Spec.Service.Annotations
	}

	desiredService := &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{
			Name:        serviceName,
			Namespace:   wss.Namespace,
			Annotations: annotations,
			Labels: map[string]string{
				"app":                     wss.Name + "-proxy",
				"soh.re/dedicatedservice": wss.Name,
				"soh.re/component":        "proxy",
			},
		},
		Spec: corev1.ServiceSpec{
			Selector: map[string]string{
				"app": wss.Name + "-proxy",
			},
			Ports: []corev1.ServicePort{
				{
					Name:       "proxy",
					Protocol:   corev1.ProtocolTCP,
					Port:       servicePort,
					TargetPort: intstr.FromInt(int(servicePort)),
				},
			},
			Type: serviceType,
		},
	}

	// Set DedicatedService instance as the owner
	if err := controllerutil.SetControllerReference(wss, desiredService, r.Scheme); err != nil {
		return err
	}

	if err != nil && errors.IsNotFound(err) {
		// Create the service
		if err := r.Create(ctx, desiredService); err != nil {
			return err
		}
		return nil
	} else if err != nil {
		return err
	}

	// Update the service if it exists (preserve ClusterIP and LoadBalancerIP)
	desiredService.Spec.ClusterIP = service.Spec.ClusterIP
	desiredService.Spec.LoadBalancerIP = service.Spec.LoadBalancerIP
	desiredService.ResourceVersion = service.ResourceVersion
	if err := r.Update(ctx, desiredService); err != nil {
		return err
	}

	return nil
}

// reconcileNetworkPolicy creates or updates a NetworkPolicy to restrict target pod network access
func (r *DedicatedServiceReconciler) reconcileNetworkPolicy(ctx context.Context, wss *sohv1alpha1.DedicatedService) error {
	logger := log.FromContext(ctx)

	policyName := wss.Name + "-netpol"
	policy := &networkingv1.NetworkPolicy{}
	err := r.Get(ctx, types.NamespacedName{Name: policyName, Namespace: wss.Namespace}, policy)

	// Define the desired NetworkPolicy
	desiredPolicy := &networkingv1.NetworkPolicy{
		ObjectMeta: metav1.ObjectMeta{
			Name:      policyName,
			Namespace: wss.Namespace,
			Labels: map[string]string{
				"app":                     wss.Name,
				"soh.re/dedicatedservice": wss.Name,
			},
		},
		Spec: networkingv1.NetworkPolicySpec{
			PodSelector: metav1.LabelSelector{
				MatchLabels: map[string]string{
					"app": wss.Name,
				},
			},
			PolicyTypes: []networkingv1.PolicyType{
				networkingv1.PolicyTypeIngress,
				networkingv1.PolicyTypeEgress,
			},
			// Allow ingress from anywhere (proxy will handle connections)
			Ingress: []networkingv1.NetworkPolicyIngressRule{
				{
					// Empty From allows from all sources
				},
			},
			// Allow egress only within the cluster
			Egress: []networkingv1.NetworkPolicyEgressRule{
				{
					// Allow DNS
					Ports: []networkingv1.NetworkPolicyPort{
						{
							Protocol: func() *corev1.Protocol { p := corev1.ProtocolUDP; return &p }(),
							Port:     &intstr.IntOrString{Type: intstr.Int, IntVal: 53},
						},
						{
							Protocol: func() *corev1.Protocol { p := corev1.ProtocolTCP; return &p }(),
							Port:     &intstr.IntOrString{Type: intstr.Int, IntVal: 53},
						},
					},
					To: []networkingv1.NetworkPolicyPeer{
						{
							// Allow to kube-dns/CoreDNS in kube-system
							NamespaceSelector: &metav1.LabelSelector{
								MatchLabels: map[string]string{
									"kubernetes.io/metadata.name": "kube-system",
								},
							},
						},
					},
				},
				{
					// Allow egress to pods within the same namespace only
					To: []networkingv1.NetworkPolicyPeer{
						{
							PodSelector: &metav1.LabelSelector{},
						},
					},
				},
			},
		},
	}

	// Set DedicatedService instance as the owner
	if err := controllerutil.SetControllerReference(wss, desiredPolicy, r.Scheme); err != nil {
		return err
	}

	if err != nil && errors.IsNotFound(err) {
		// Create the NetworkPolicy
		logger.Info("Creating NetworkPolicy", "name", policyName)
		if err := r.Create(ctx, desiredPolicy); err != nil {
			return err
		}
		return nil
	} else if err != nil {
		return err
	}

	// Update the NetworkPolicy if it exists
	desiredPolicy.ResourceVersion = policy.ResourceVersion
	if err := r.Update(ctx, desiredPolicy); err != nil {
		return err
	}

	return nil
}

func resourceQuantity(value string) *resource.Quantity {
	q, _ := resource.ParseQuantity(value)
	return &q
}

// SetupWithManager sets up the controller with the Manager.
func (r *DedicatedServiceReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&sohv1alpha1.DedicatedService{}).
		Owns(&appsv1.Deployment{}).
		Owns(&corev1.Service{}).
		Owns(&networkingv1.NetworkPolicy{}).
		Complete(r)
}
