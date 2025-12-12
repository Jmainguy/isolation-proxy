package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// IsolationPodSpec defines the desired state of IsolationPod
type IsolationPodSpec struct {
	// ServiceName is the name of the DedicatedService this pod belongs to
	ServiceName string `json:"serviceName"`

	// PodName is the name of the underlying Kubernetes pod
	PodName string `json:"podName"`

	// PodIP is the IP address of the pod
	PodIP string `json:"podIP,omitempty"`

	// Port is the port the pod is listening on
	Port int32 `json:"port"`
}

// IsolationPodStatus defines the observed state of IsolationPod
type IsolationPodStatus struct {
	// InUse indicates if this pod is currently serving a connection
	InUse bool `json:"inUse"`

	// ConnectionStartTime is when the current connection started (if InUse)
	// +optional
	ConnectionStartTime *metav1.Time `json:"connectionStartTime,omitempty"`

	// LastUsedTime is when this pod was last used
	// +optional
	LastUsedTime *metav1.Time `json:"lastUsedTime,omitempty"`

	// Phase indicates the lifecycle phase of this isolation pod
	// +optional
	Phase string `json:"phase,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:shortName={ipod,ipods}
// +kubebuilder:printcolumn:name="Service",type=string,JSONPath=`.spec.serviceName`
// +kubebuilder:printcolumn:name="Pod",type=string,JSONPath=`.spec.podName`
// +kubebuilder:printcolumn:name="In-Use",type=boolean,JSONPath=`.status.inUse`
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`

// IsolationPod represents a single pod instance managed for connection isolation
type IsolationPod struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   IsolationPodSpec   `json:"spec,omitempty"`
	Status IsolationPodStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true

// IsolationPodList contains a list of IsolationPod
type IsolationPodList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []IsolationPod `json:"items"`
}

func init() {
	SchemeBuilder.Register(&IsolationPod{}, &IsolationPodList{})
}
