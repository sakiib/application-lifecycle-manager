/*
Copyright 2025.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package v1alpha1

import (
	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// ApplicationSpec defines the desired state of Application
type ApplicationSpec struct {
	// Replicas is the number of desired pods for the Deployment.
	// +kubebuilder:validation:Minimum=0
	// +kubebuilder:default=1
	// +optional
	Replicas *int32 `json:"replicas,omitempty"`

	// Image is the container image for the Deployment.
	// +kubebuilder:validation:Required
	Image string `json:"image"`

	// ContainerPort is the port the container exposes.
	// +kubebuilder:validation:Minimum=1
	// +kubebuilder:validation:Maximum=65535
	// +kubebuilder:default=80
	// +optional
	ContainerPort *int32 `json:"containerPort,omitempty"`

	// ServiceSpec defines the parameters for the Service.
	// +optional
	Service *ApplicationServiceSpec `json:"service,omitempty"`

	// IngressSpec defines the parameters for the Ingress.
	// If nil, no Ingress will be created.
	// +optional
	Ingress *ApplicationIngressSpec `json:"ingress,omitempty"`

	// EnvVars allows defining environment variables for the application pods.
	// +optional
	EnvVars []corev1.EnvVar `json:"envVars,omitempty"`

	// Resources allows defining resource requests and limits for the application pods.
	// +optional
	Resources *corev1.ResourceRequirements `json:"resources,omitempty"`

	// LivenessProbe defines the liveness probe for the application pods.
	// +optional
	LivenessProbe *corev1.Probe `json:"livenessProbe,omitempty"`

	// ReadinessProbe defines the readiness probe for the application pods.
	// +optional
	ReadinessProbe *corev1.Probe `json:"readinessProbe,omitempty"`
}

// ApplicationServiceSpec defines parameters for the Service
type ApplicationServiceSpec struct {
	// Port is the port the Service will expose.
	// Defaults to the value of ApplicationSpec.ContainerPort.
	// +kubebuilder:validation:Minimum=1
	// +kubebuilder:validation:Maximum=65535
	// +optional
	Port *int32 `json:"port,omitempty"`

	// Type is the type of Service to create (e.g., ClusterIP, NodePort, LoadBalancer). Only ClusterIP is allowed atm
	// +kubebuilder:validation:Enum=ClusterIP
	// +kubebuilder:default=ClusterIP
	// +optional
	Type *corev1.ServiceType `json:"type,omitempty"`
}

// ApplicationIngressSpec defines parameters for the Ingress
type ApplicationIngressSpec struct {
	// Host is the hostname for the Ingress rule.
	// +kubebuilder:validation:Required
	Host string `json:"host"`

	// Path for the Ingress rule.
	// +kubebuilder:default="/"
	// +optional
	Path string `json:"path,omitempty"`

	// PathType for the Ingress rule.
	// +kubebuilder:validation:Enum=Exact;Prefix;ImplementationSpecific
	// +kubebuilder:default=Prefix
	// +optional
	PathType *networkingv1.PathType `json:"pathType,omitempty"`

	// IngressClassName specifies the IngressClass resource name.
	// +optional
	IngressClassName *string `json:"ingressClassName,omitempty"`

	// Annotations for the Ingress resource.
	// +optional
	Annotations map[string]string `json:"annotations,omitempty"`

	// TLS configuration for the Ingress.
	// +optional
	TLS []networkingv1.IngressTLS `json:"tls,omitempty"`
}

// ApplicationStatus defines the observed state of Application
type ApplicationStatus struct {
	// ObservedGeneration is the last generation reconciled by the controller.
	// +optional
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`

	// DeploymentName is the name of the managed Deployment.
	// +optional
	DeploymentName string `json:"deploymentName,omitempty"`

	// ServiceName is the name of the managed Service.
	// +optional
	ServiceName string `json:"serviceName,omitempty"`

	// IngressName is the name of the managed Ingress.
	// +optional
	IngressName string `json:"ingressName,omitempty"`

	// IngressURL is the primary URL exposed by the Ingress.
	// +optional
	IngressURL string `json:"ingressURL,omitempty"`

	// AvailableReplicas is the number of available replicas for the Deployment.
	// +optional
	AvailableReplicas int32 `json:"availableReplicas,omitempty"`

	// Conditions represent the latest available observations of an object's state.
	// +optional
	// +patchMergeKey=type
	// +patchStrategy=merge
	Conditions []metav1.Condition `json:"conditions,omitempty" patchStrategy:"merge" patchMergeKey:"type"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:printcolumn:name="Image",type=string,JSONPath=`.spec.image`
// +kubebuilder:printcolumn:name="Replicas",type=integer,JSONPath=`.spec.replicas`
// +kubebuilder:printcolumn:name="IngressURL",type=string,JSONPath=`.status.ingressURL`
// +kubebuilder:printcolumn:name="AppStatus",type=string,JSONPath=`.status.conditions[?(@.type=="Ready")].status`
// +kubebuilder:printcolumn:name="Age",type="date",JSONPath=".metadata.creationTimestamp"
// Application is the Schema for the applications API
type Application struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   ApplicationSpec   `json:"spec,omitempty"`
	Status ApplicationStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true
// ApplicationList contains a list of Application
type ApplicationList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []Application `json:"items"`
}

func init() {
	SchemeBuilder.Register(&Application{}, &ApplicationList{})
}
