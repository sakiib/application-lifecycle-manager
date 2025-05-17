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

package controller

import (
	"context"
	"fmt"
	"reflect"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/client-go/tools/record"
	"k8s.io/client-go/util/retry"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/log"

	appsv1alpha1 "github.com/sakiib/application-lifecycle-manager/api/v1alpha1"
)

const (
	applicationFinalizer        = "apps.example.com/finalizer" // ADJUST YOUR DOMAIN (e.g., apps.your-company.com/finalizer)
	ConditionAvailable          = "Available"
	ConditionProgressing        = "Progressing"
	ConditionDegraded           = "Degraded"
	ReasonComponentsReady       = "ComponentsReady"
	ReasonComponentsNotReady    = "ComponentsNotReady"
	ReasonDeploymentCreated     = "DeploymentCreated"
	ReasonDeploymentUpdated     = "DeploymentUpdated"
	ReasonDeploymentProgressing = "DeploymentProgressing"
	ReasonDeploymentFailed      = "DeploymentFailed"
	ReasonServiceCreated        = "ServiceCreated"
	ReasonServiceUpdated        = "ServiceUpdated"
	ReasonServiceError          = "ServiceError"
	ReasonIngressCreated        = "IngressCreated"
	ReasonIngressUpdated        = "IngressUpdated"
	ReasonIngressDeleted        = "IngressDeleted"
	ReasonIngressError          = "IngressError"
)

// ApplicationReconciler reconciles a Application object
type ApplicationReconciler struct {
	client.Client
	Scheme   *runtime.Scheme
	Recorder record.EventRecorder
}

// +kubebuilder:rbac:groups=apps.example.com,resources=applications,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=apps.example.com,resources=applications/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=apps.example.com,resources=applications/finalizers,verbs=update
// +kubebuilder:rbac:groups=apps,resources=deployments,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups="",resources=services,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=networking.k8s.io,resources=ingresses,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups="",resources=events,verbs=create;patch
// +kubebuilder:rbac:groups="coordination.k8s.io",resources=leases,verbs=get;list;watch;create;update;patch;delete

// updateFullStatus updates the entire status subresource, retrying on conflict.
func (r *ApplicationReconciler) updateFullStatus(ctx context.Context, appCR *appsv1alpha1.Application, desiredStatus *appsv1alpha1.ApplicationStatus) (ctrl.Result, error) {
	logger := log.FromContext(ctx)

	// Check if desired status is actually different from appCR.Status.
	// This helps avoid unnecessary API calls if status calculation resulted in no effective change.
	// Also check ObservedGeneration because even if other fields are same, if OG changed, we must update.
	if reflect.DeepEqual(appCR.Status, *desiredStatus) && appCR.Status.ObservedGeneration == desiredStatus.ObservedGeneration {
		logger.V(1).Info("Status is unchanged, skipping update.", "application", appCR.Name)
		return ctrl.Result{}, nil
	}

	err := retry.RetryOnConflict(retry.DefaultRetry, func() error {
		latestCR := &appsv1alpha1.Application{}
		if getErr := r.Get(ctx, types.NamespacedName{Name: appCR.Name, Namespace: appCR.Namespace}, latestCR); getErr != nil {
			logger.Error(getErr, "Failed to get latest Application for status update")
			return getErr
		}
		latestCR.Status = *desiredStatus // Apply the desired status changes
		updateErr := r.Status().Update(ctx, latestCR)
		if updateErr != nil {
			logger.V(1).Info("Conflict during status update, retrying...", "error", updateErr.Error())
		}
		return updateErr
	})

	if err != nil {
		logger.Error(err, "Failed to update Application status after multiple retries")
		return ctrl.Result{}, err // Return error to requeue
	}
	logger.Info("Successfully updated Application status", "application", appCR.Name)
	return ctrl.Result{}, nil
}

func (r *ApplicationReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	logger := log.FromContext(ctx)
	logger.Info("Reconciling Application", "request", req.NamespacedName)

	app := &appsv1alpha1.Application{}
	if err := r.Get(ctx, req.NamespacedName, app); err != nil {
		if apierrors.IsNotFound(err) {
			logger.Info("Application resource not found. Ignoring since object must be deleted.")
			return ctrl.Result{}, nil
		}
		logger.Error(err, "Failed to get Application")
		return ctrl.Result{}, err
	}

	currentStatus := app.Status.DeepCopy() // Work on a copy for modifications
	statusNeedsUpdate := false             // Track if status has changed

	if currentStatus.Conditions == nil {
		currentStatus.Conditions = []metav1.Condition{}
		statusNeedsUpdate = true
	}

	r.applySpecDefaults(app) // Modifies in-memory app.Spec

	// Handle finalizer
	if app.ObjectMeta.DeletionTimestamp.IsZero() {
		if !controllerutil.ContainsFinalizer(app, applicationFinalizer) {
			logger.Info("Adding Finalizer for Application", "name", app.Name)
			controllerutil.AddFinalizer(app, applicationFinalizer)
			if err := r.Update(ctx, app); err != nil {
				logger.Error(err, "Failed to add finalizer")
				return ctrl.Result{}, err
			}
			return ctrl.Result{Requeue: true}, nil
		}
	} else { // Object is being deleted
		if controllerutil.ContainsFinalizer(app, applicationFinalizer) {
			logger.Info("Application is being deleted, performing cleanup", "name", app.Name)
			// For this example, we rely on OwnerReferences for owned K8s resources.
			// Add explicit cleanup for external resources if any.
			controllerutil.RemoveFinalizer(app, applicationFinalizer)
			if err := r.Update(ctx, app); err != nil {
				logger.Error(err, "Failed to remove finalizer")
				return ctrl.Result{}, err
			}
			logger.Info("Finalizer removed", "name", app.Name)
		}
		return ctrl.Result{}, nil
	}

	// Reconcile Deployment
	deployment, err := r.reconcileDeployment(ctx, app)
	if err != nil {
		logger.Error(err, "Failed to reconcile Deployment")
		errMsg := "Deployment reconciliation failed: " + err.Error()
		if setApplicationCondition(currentStatus, ConditionDegraded, metav1.ConditionTrue, ReasonDeploymentFailed, errMsg, app.Generation) {
			statusNeedsUpdate = true
		}
		if setApplicationCondition(currentStatus, ConditionAvailable, metav1.ConditionFalse, ReasonDeploymentFailed, errMsg, app.Generation) {
			statusNeedsUpdate = true
		}
		if setApplicationCondition(currentStatus, ConditionProgressing, metav1.ConditionFalse, ReasonDeploymentFailed, errMsg, app.Generation) {
			statusNeedsUpdate = true
		}
		if currentStatus.DeploymentName != "" {
			currentStatus.DeploymentName = ""
			statusNeedsUpdate = true
		}
		if currentStatus.AvailableReplicas != 0 {
			currentStatus.AvailableReplicas = 0
			statusNeedsUpdate = true
		}

		if statusNeedsUpdate {
			_, _ = r.updateFullStatus(ctx, app, currentStatus)
		}
		return ctrl.Result{}, err // Return original error for requeue
	}
	// Update status based on successful deployment reconciliation
	if currentStatus.DeploymentName != deployment.Name {
		currentStatus.DeploymentName = deployment.Name
		statusNeedsUpdate = true
	}
	if currentStatus.AvailableReplicas != deployment.Status.AvailableReplicas {
		currentStatus.AvailableReplicas = deployment.Status.AvailableReplicas
		statusNeedsUpdate = true
	}
	if updateConditionsFromDeployment(deployment, currentStatus, app.Generation) {
		statusNeedsUpdate = true
	}

	// Reconcile Service
	service, err := r.reconcileService(ctx, app)
	if err != nil {
		logger.Error(err, "Failed to reconcile Service")
		errMsg := "Service reconciliation failed: " + err.Error()
		if setApplicationCondition(currentStatus, ConditionDegraded, metav1.ConditionTrue, ReasonServiceError, errMsg, app.Generation) {
			statusNeedsUpdate = true
		}
		if currentStatus.ServiceName != "" {
			currentStatus.ServiceName = ""
			statusNeedsUpdate = true
		}
		if statusNeedsUpdate {
			_, _ = r.updateFullStatus(ctx, app, currentStatus)
		}
		return ctrl.Result{}, err
	}
	if currentStatus.ServiceName != service.Name {
		currentStatus.ServiceName = service.Name
		statusNeedsUpdate = true
	}

	// Reconcile Ingress (if specified)
	if app.Spec.Ingress != nil {
		ingress, err := r.reconcileIngress(ctx, app, service.Name)
		if err != nil {
			logger.Error(err, "Failed to reconcile Ingress")
			errMsg := "Ingress reconciliation failed: " + err.Error()
			if setApplicationCondition(currentStatus, ConditionDegraded, metav1.ConditionTrue, ReasonIngressError, errMsg, app.Generation) {
				statusNeedsUpdate = true
			}
			if currentStatus.IngressName != "" {
				currentStatus.IngressName = ""
				statusNeedsUpdate = true
			}
			if currentStatus.IngressURL != "" {
				currentStatus.IngressURL = ""
				statusNeedsUpdate = true
			}
			if statusNeedsUpdate {
				_, _ = r.updateFullStatus(ctx, app, currentStatus)
			}
			return ctrl.Result{}, err
		}
		if currentStatus.IngressName != ingress.Name {
			currentStatus.IngressName = ingress.Name
			statusNeedsUpdate = true
		}

		var newIngressURL string
		if len(ingress.Spec.Rules) > 0 && ingress.Spec.Rules[0].Host != "" { // Ensure host is present
			scheme := "http"
			if len(ingress.Spec.TLS) > 0 {
				scheme = "https"
			}
			path := "/"
			if len(ingress.Spec.Rules[0].HTTP.Paths) > 0 {
				path = ingress.Spec.Rules[0].HTTP.Paths[0].Path
			}
			newIngressURL = fmt.Sprintf("%s://%s%s", scheme, ingress.Spec.Rules[0].Host, path)
		}
		if currentStatus.IngressURL != newIngressURL {
			currentStatus.IngressURL = newIngressURL
			statusNeedsUpdate = true
		}

	} else { // Ingress not specified, ensure it's cleaned up
		if err := r.ensureIngressDeleted(ctx, app); err != nil {
			logger.Error(err, "Failed to ensure Ingress is deleted")
			errMsg := "Failed to delete old ingress: " + err.Error()
			if setApplicationCondition(currentStatus, ConditionDegraded, metav1.ConditionTrue, ReasonIngressError, errMsg, app.Generation) {
				statusNeedsUpdate = true
			}
		}
		if currentStatus.IngressName != "" {
			currentStatus.IngressName = ""
			statusNeedsUpdate = true
		}
		if currentStatus.IngressURL != "" {
			currentStatus.IngressURL = ""
			statusNeedsUpdate = true
		}
	}

	// Set Overall Ready/Available/Degraded conditions
	if isAppReady(currentStatus) {
		if setApplicationCondition(currentStatus, ConditionDegraded, metav1.ConditionFalse, ReasonComponentsReady, "All components are provisioned and appear ready.", app.Generation) {
			statusNeedsUpdate = true
		}
		if setApplicationCondition(currentStatus, ConditionAvailable, metav1.ConditionTrue, ReasonComponentsReady, "Application is available.", app.Generation) {
			statusNeedsUpdate = true
		}
	} else {
		// If not ready, ensure Available is false. Degraded might already be true due to specific component issues.
		if setApplicationCondition(currentStatus, ConditionAvailable, metav1.ConditionFalse, ReasonComponentsNotReady, "Application is not fully available or progressing.", app.Generation) {
			statusNeedsUpdate = true
		}
		// We don't blindly set Degraded to true here; individual component errors should do that.
		// If Progressing is false and Available is false, it implies an issue.
		isProgressing := false
		for _, cond := range currentStatus.Conditions {
			if cond.Type == ConditionProgressing && cond.Status == metav1.ConditionTrue {
				isProgressing = true
				break
			}
		}
		if !isProgressing && !isAppReady(currentStatus) { // If not progressing and not ready, consider it degraded.
			// Check if a Degraded=true condition already exists from a specific component failure.
			degradedExists := false
			for _, cond := range currentStatus.Conditions {
				if cond.Type == ConditionDegraded && cond.Status == metav1.ConditionTrue {
					degradedExists = true
					break
				}
			}
			if !degradedExists {
				if setApplicationCondition(currentStatus, ConditionDegraded, metav1.ConditionTrue, ReasonComponentsNotReady, "One or more components are not ready or progressing.", app.Generation) {
					statusNeedsUpdate = true
				}
			}
		}
	}

	if currentStatus.ObservedGeneration != app.Generation {
		currentStatus.ObservedGeneration = app.Generation
		statusNeedsUpdate = true
	}

	var finalErr error
	var finalResult ctrl.Result = ctrl.Result{} // Default result

	if statusNeedsUpdate {
		logger.Info("Status requires update.", "application", app.Name)
		finalResult, finalErr = r.updateFullStatus(ctx, app, currentStatus)
		if finalErr != nil {
			return finalResult, finalErr // Error from status update
		}
	} else {
		logger.V(1).Info("No status changes detected for this reconciliation.", "application", app.Name)
	}

	// Requeue if deployment is not fully available yet
	if deployment != nil && deployment.Status.AvailableReplicas < *app.Spec.Replicas {
		requeueDelay := 15 * time.Second
		logger.Info("Deployment not fully available, requeuing.", "desired", *app.Spec.Replicas, "available", deployment.Status.AvailableReplicas, "requeueAfter", requeueDelay)
		// If finalErr was nil from status update (or no status update happened), this requeue takes precedence
		return ctrl.Result{RequeueAfter: requeueDelay}, finalErr
	}

	return finalResult, finalErr // Return result from status update (if any) or default empty result
}

func (r *ApplicationReconciler) applySpecDefaults(app *appsv1alpha1.Application) {
	changed := false // To track if any default was actually applied
	if app.Spec.Replicas == nil {
		one := int32(1)
		app.Spec.Replicas = &one
		changed = true
	}
	if app.Spec.ContainerPort == nil {
		defaultPort := int32(80)
		app.Spec.ContainerPort = &defaultPort
		changed = true
	}
	if app.Spec.Service != nil {
		if app.Spec.Service.Port == nil {
			// Ensure ContainerPort has been defaulted before using it here
			containerPort := int32(80)
			if app.Spec.ContainerPort != nil {
				containerPort = *app.Spec.ContainerPort
			}
			app.Spec.Service.Port = &containerPort
			changed = true
		}
		if app.Spec.Service.Type == nil {
			defaultServiceType := corev1.ServiceTypeClusterIP
			app.Spec.Service.Type = &defaultServiceType
			changed = true
		}
	}
	if app.Spec.Ingress != nil {
		if app.Spec.Ingress.Path == "" {
			app.Spec.Ingress.Path = "/"
			changed = true
		}
		if app.Spec.Ingress.PathType == nil {
			defaultPathType := networkingv1.PathTypePrefix
			app.Spec.Ingress.PathType = &defaultPathType
			changed = true
		}
	}
	if changed {
		log.Log.V(1).Info("Applied defaults to in-memory Application spec for processing", "application", app.Name)
	}
}

func (r *ApplicationReconciler) reconcileDeployment(ctx context.Context, app *appsv1alpha1.Application) (*appsv1.Deployment, error) {
	logger := log.FromContext(ctx)
	deploymentName := app.Name + "-deployment"

	desiredDeployment := &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{
			Name:      deploymentName,
			Namespace: app.Namespace,
			Labels:    r.getAppLabels(app, "deployment"),
		},
		Spec: appsv1.DeploymentSpec{
			Replicas: app.Spec.Replicas, // Assumes applySpecDefaults has run
			Selector: &metav1.LabelSelector{
				MatchLabels: r.getSelectorLabels(app),
			},
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{
					Labels: r.getSelectorLabels(app),
				},
				Spec: corev1.PodSpec{
					Containers: []corev1.Container{{
						Name:  app.Name,
						Image: app.Spec.Image,
						Ports: []corev1.ContainerPort{{
							ContainerPort: *app.Spec.ContainerPort, // Assumes applySpecDefaults
						}},
						Env:            app.Spec.EnvVars,
						Resources:      safeResourceRequirements(app.Spec.Resources),
						LivenessProbe:  app.Spec.LivenessProbe,
						ReadinessProbe: app.Spec.ReadinessProbe,
					}},
				},
			},
		},
	}

	if err := controllerutil.SetControllerReference(app, desiredDeployment, r.Scheme); err != nil {
		return nil, fmt.Errorf("failed to set owner reference on Deployment: %w", err)
	}

	foundDeployment := &appsv1.Deployment{}
	err := r.Get(ctx, types.NamespacedName{Name: deploymentName, Namespace: app.Namespace}, foundDeployment)
	if err != nil && apierrors.IsNotFound(err) {
		logger.Info("Creating a new Deployment", "Deployment.Namespace", desiredDeployment.Namespace, "Deployment.Name", desiredDeployment.Name)
		err = r.Create(ctx, desiredDeployment)
		if err != nil {
			return nil, fmt.Errorf("failed to create Deployment: %w", err)
		}
		r.Recorder.Event(app, corev1.EventTypeNormal, ReasonDeploymentCreated, fmt.Sprintf("Created Deployment %s/%s", desiredDeployment.Namespace, desiredDeployment.Name))
		return desiredDeployment, nil
	} else if err != nil {
		return nil, fmt.Errorf("failed to get Deployment: %w", err)
	}

	// Check for updates - simplistic comparison, consider using a more robust diff or patch
	needsUpdate := false
	if !reflect.DeepEqual(foundDeployment.Spec.Replicas, desiredDeployment.Spec.Replicas) {
		needsUpdate = true
		foundDeployment.Spec.Replicas = desiredDeployment.Spec.Replicas
	}
	if foundDeployment.Spec.Template.Spec.Containers[0].Image != desiredDeployment.Spec.Template.Spec.Containers[0].Image {
		needsUpdate = true
		foundDeployment.Spec.Template.Spec.Containers[0].Image = desiredDeployment.Spec.Template.Spec.Containers[0].Image
	}
	// Compare other mutable fields from your spec: Ports, EnvVars, Resources, Probes
	if !reflect.DeepEqual(foundDeployment.Spec.Template.Spec.Containers[0].Ports, desiredDeployment.Spec.Template.Spec.Containers[0].Ports) {
		needsUpdate = true
		foundDeployment.Spec.Template.Spec.Containers[0].Ports = desiredDeployment.Spec.Template.Spec.Containers[0].Ports
	}
	if !reflect.DeepEqual(foundDeployment.Spec.Template.Spec.Containers[0].Env, desiredDeployment.Spec.Template.Spec.Containers[0].Env) {
		needsUpdate = true
		foundDeployment.Spec.Template.Spec.Containers[0].Env = desiredDeployment.Spec.Template.Spec.Containers[0].Env
	}
	if !reflect.DeepEqual(foundDeployment.Spec.Template.Spec.Containers[0].Resources, desiredDeployment.Spec.Template.Spec.Containers[0].Resources) {
		needsUpdate = true
		foundDeployment.Spec.Template.Spec.Containers[0].Resources = desiredDeployment.Spec.Template.Spec.Containers[0].Resources
	}
	if !reflect.DeepEqual(foundDeployment.Spec.Template.Spec.Containers[0].LivenessProbe, desiredDeployment.Spec.Template.Spec.Containers[0].LivenessProbe) {
		needsUpdate = true
		foundDeployment.Spec.Template.Spec.Containers[0].LivenessProbe = desiredDeployment.Spec.Template.Spec.Containers[0].LivenessProbe
	}
	if !reflect.DeepEqual(foundDeployment.Spec.Template.Spec.Containers[0].ReadinessProbe, desiredDeployment.Spec.Template.Spec.Containers[0].ReadinessProbe) {
		needsUpdate = true
		foundDeployment.Spec.Template.Spec.Containers[0].ReadinessProbe = desiredDeployment.Spec.Template.Spec.Containers[0].ReadinessProbe
	}
	// Also compare labels and annotations on the Deployment itself
	if !reflect.DeepEqual(foundDeployment.Labels, desiredDeployment.Labels) {
		needsUpdate = true
		foundDeployment.Labels = desiredDeployment.Labels
	}
	if !reflect.DeepEqual(foundDeployment.Annotations, desiredDeployment.Annotations) { // Note: K8s might add its own annotations
		// More careful annotation comparison might be needed if you only care about user-defined ones.
		// For now, let's assume direct comparison is okay or that desiredDeployment.Annotations only contains what you manage.
		// foundDeployment.Annotations = desiredDeployment.Annotations // This could overwrite K8s added ones
		// needsUpdate = true
	}

	if needsUpdate {
		logger.Info("Updating existing Deployment", "Deployment.Namespace", foundDeployment.Namespace, "Deployment.Name", foundDeployment.Name)
		err = r.Update(ctx, foundDeployment)
		if err != nil {
			return nil, fmt.Errorf("failed to update Deployment: %w", err)
		}
		r.Recorder.Event(app, corev1.EventTypeNormal, ReasonDeploymentUpdated, fmt.Sprintf("Updated Deployment %s/%s", foundDeployment.Namespace, foundDeployment.Name))
		return foundDeployment, nil
	}

	logger.V(1).Info("Deployment is up-to-date", "Deployment.Namespace", foundDeployment.Namespace, "Deployment.Name", foundDeployment.Name)
	return foundDeployment, nil
}

// internal/controller/application_controller.go

// ... (other imports and a_controller.gopackage controller code) ...

func (r *ApplicationReconciler) reconcileService(ctx context.Context, app *appsv1alpha1.Application) (*corev1.Service, error) {
	logger := log.FromContext(ctx)
	serviceName := app.Name + "-service" // Or derive from app.Spec if you add a field for it

	// Determine the service port. Default to containerPort if not specified in app.Spec.Service.
	servicePort := *app.Spec.ContainerPort // Assumes ContainerPort is defaulted if nil
	if app.Spec.Service != nil && app.Spec.Service.Port != nil {
		servicePort = *app.Spec.Service.Port
	}

	// ---- Define the Desired Service State (Always ClusterIP) ----
	desiredService := &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{
			Name:      serviceName,
			Namespace: app.Namespace,
			Labels:    r.getAppLabels(app, "service"),
			// Annotations can be added here if needed
		},
		Spec: corev1.ServiceSpec{
			Ports: []corev1.ServicePort{{
				Name:       "http", // Good practice to name ports
				Port:       servicePort,
				TargetPort: intstr.FromInt32(*app.Spec.ContainerPort), // Target container port
				Protocol:   corev1.ProtocolTCP,
			}},
			Selector: r.getSelectorLabels(app),    // Selects pods managed by this app's deployment
			Type:     corev1.ServiceTypeClusterIP, // MODIFIED: Always ClusterIP
		},
	}

	// Set Application instance as the owner and controller
	if err := controllerutil.SetControllerReference(app, desiredService, r.Scheme); err != nil {
		return nil, fmt.Errorf("failed to set owner reference on Service: %w", err)
	}

	// ---- Check if the Service already exists ----
	foundService := &corev1.Service{}
	err := r.Get(ctx, types.NamespacedName{Name: serviceName, Namespace: app.Namespace}, foundService)

	if err != nil && apierrors.IsNotFound(err) {
		// ---- Create the Service if it does not exist ----
		logger.Info("Creating a new Service", "Service.Namespace", desiredService.Namespace, "Service.Name", desiredService.Name, "Type", desiredService.Spec.Type)
		err = r.Create(ctx, desiredService)
		if err != nil {
			r.Recorder.Eventf(app, corev1.EventTypeWarning, ReasonServiceError, "Failed to create Service %s: %v", desiredService.Name, err)
			return nil, fmt.Errorf("failed to create Service: %w", err)
		}
		r.Recorder.Eventf(app, corev1.EventTypeNormal, ReasonServiceCreated, "Created Service %s/%s", desiredService.Namespace, desiredService.Name)
		return desiredService, nil
	} else if err != nil {
		// Other error than NotFound
		logger.Error(err, "Failed to get Service")
		return nil, fmt.Errorf("failed to get Service: %w", err)
	}

	// ---- Service Exists - Ensure it matches the desired state (ClusterIP type and ports) ----
	needsUpdate := false

	// 1. Check Service Type (should always be ClusterIP now)
	if foundService.Spec.Type != corev1.ServiceTypeClusterIP {
		logger.Info("Service type mismatch, will update to ClusterIP", "Service.Name", foundService.Name, "FoundType", foundService.Spec.Type)
		foundService.Spec.Type = corev1.ServiceTypeClusterIP
		// When changing type to ClusterIP, K8s might clear NodePort/LoadBalancerIPs.
		// For ClusterIP, we want to preserve an existing ClusterIP if one was allocated.
		// However, if foundService.Spec.ClusterIP is empty, K8s will assign one.
		// If changing from LoadBalancer/NodePort, K8s handles cleanup of those specific fields.
		// We don't need to explicitly clear foundService.Spec.ClusterIP here if changing TO ClusterIP.
		needsUpdate = true
	}

	// 2. Check Ports (assuming one port for simplicity)
	// Ensure labels and selector match too, as they are critical.
	desiredPorts := desiredService.Spec.Ports
	if len(foundService.Spec.Ports) != 1 ||
		foundService.Spec.Ports[0].Port != desiredPorts[0].Port ||
		foundService.Spec.Ports[0].TargetPort.IntVal != desiredPorts[0].TargetPort.IntVal ||
		foundService.Spec.Ports[0].Protocol != desiredPorts[0].Protocol ||
		foundService.Spec.Ports[0].Name != desiredPorts[0].Name {
		logger.Info("Service port configuration mismatch, will update.", "Service.Name", foundService.Name)
		foundService.Spec.Ports = desiredPorts
		needsUpdate = true
	}

	// 3. Check Selector
	if !reflect.DeepEqual(foundService.Spec.Selector, desiredService.Spec.Selector) {
		logger.Info("Service selector mismatch, will update.", "Service.Name", foundService.Name)
		foundService.Spec.Selector = desiredService.Spec.Selector
		needsUpdate = true
	}

	// 4. Check Labels (managed by this controller)
	if !reflect.DeepEqual(foundService.Labels, desiredService.Labels) {
		logger.Info("Service labels mismatch, will update.", "Service.Name", foundService.Name)
		foundService.Labels = desiredService.Labels // Ensure controller managed labels are present
		needsUpdate = true
	}

	if needsUpdate {
		logger.Info("Updating existing Service", "Service.Namespace", foundService.Namespace, "Service.Name", foundService.Name)
		// Preserve the existing ClusterIP if the service type is already ClusterIP or is being set to ClusterIP
		// and an IP was already allocated. This is important because ClusterIP is immutable after creation by some CNI.
		// However, if we are changing other fields, we should let K8s handle it.
		// The simple foundService.Spec = desiredService.Spec might be too blunt if ClusterIP needs preservation.
		// Let's explicitly copy fields:
		existingClusterIP := foundService.Spec.ClusterIP // Save before overwriting spec parts

		foundService.Spec.Ports = desiredService.Spec.Ports
		foundService.Spec.Selector = desiredService.Spec.Selector
		foundService.Spec.Type = corev1.ServiceTypeClusterIP // Explicitly ensure type

		// If it was already ClusterIP and had an IP, keep it. Otherwise, let K8s assign one.
		if foundService.Spec.Type == corev1.ServiceTypeClusterIP {
			foundService.Spec.ClusterIP = existingClusterIP
		} else {
			// This case should ideally not be hit if we are always reconciling to ClusterIP
			// and if the type was different, we've already set foundService.Spec.Type to ClusterIP.
			// If it was some other type before and had a clusterIP (unlikely but possible),
			// setting Type to ClusterIP and ClusterIP to "" lets K8s allocate a new one.
			// foundService.Spec.ClusterIP = "" // Let K8s allocate if it wasn't ClusterIP before
		}

		err = r.Update(ctx, foundService)
		if err != nil {
			r.Recorder.Eventf(app, corev1.EventTypeWarning, ReasonServiceError, "Failed to update Service %s: %v", foundService.Name, err)
			return nil, fmt.Errorf("failed to update Service: %w", err)
		}
		r.Recorder.Eventf(app, corev1.EventTypeNormal, ReasonServiceUpdated, "Updated Service %s/%s", foundService.Namespace, foundService.Name)
		return foundService, nil // Return the updated service
	}

	logger.V(1).Info("Service is up-to-date", "Service.Namespace", foundService.Namespace, "Service.Name", foundService.Name)
	return foundService, nil // Return existing, up-to-date service
}

func (r *ApplicationReconciler) reconcileIngress(ctx context.Context, app *appsv1alpha1.Application, serviceName string) (*networkingv1.Ingress, error) {
	logger := log.FromContext(ctx)
	ingressName := app.Name + "-ingress"

	// Values should be defaulted by applySpecDefaults if app.Spec.Ingress is not nil
	ingressSpec := app.Spec.Ingress
	if ingressSpec == nil { // Should be checked by caller, defensive
		return nil, nil // Or an error: return nil, fmt.Errorf("ingress spec is nil")
	}

	pathType := *ingressSpec.PathType // Defaulted by applySpecDefaults
	path := ingressSpec.Path          // Defaulted

	servicePortNumber := *app.Spec.ContainerPort // Default backend port to containerPort
	if app.Spec.Service != nil && app.Spec.Service.Port != nil {
		servicePortNumber = *app.Spec.Service.Port // Use specified service port if available
	}

	desiredIngress := &networkingv1.Ingress{
		ObjectMeta: metav1.ObjectMeta{
			Name: ingressName, Namespace: app.Namespace,
			Labels:      r.getAppLabels(app, "ingress"),
			Annotations: ingressSpec.Annotations,
		},
		Spec: networkingv1.IngressSpec{
			IngressClassName: ingressSpec.IngressClassName,
			TLS:              ingressSpec.TLS,
			Rules: []networkingv1.IngressRule{{
				Host: ingressSpec.Host,
				IngressRuleValue: networkingv1.IngressRuleValue{
					HTTP: &networkingv1.HTTPIngressRuleValue{
						Paths: []networkingv1.HTTPIngressPath{{
							Path:     path,
							PathType: &pathType,
							Backend: networkingv1.IngressBackend{
								Service: &networkingv1.IngressServiceBackend{
									Name: serviceName,
									Port: networkingv1.ServiceBackendPort{Number: servicePortNumber},
								},
							},
						}},
					},
				},
			}},
		},
	}
	if err := controllerutil.SetControllerReference(app, desiredIngress, r.Scheme); err != nil {
		return nil, fmt.Errorf("failed to set owner reference on Ingress: %w", err)
	}

	foundIngress := &networkingv1.Ingress{}
	err := r.Get(ctx, types.NamespacedName{Name: ingressName, Namespace: app.Namespace}, foundIngress)
	if err != nil && apierrors.IsNotFound(err) {
		logger.Info("Creating a new Ingress", "Ingress.Namespace", desiredIngress.Namespace, "Ingress.Name", desiredIngress.Name)
		err = r.Create(ctx, desiredIngress)
		if err != nil {
			return nil, fmt.Errorf("failed to create Ingress: %w", err)
		}
		r.Recorder.Event(app, corev1.EventTypeNormal, ReasonIngressCreated, fmt.Sprintf("Created Ingress %s/%s", desiredIngress.Namespace, desiredIngress.Name))
		return desiredIngress, nil
	} else if err != nil {
		return nil, fmt.Errorf("failed to get Ingress: %w", err)
	}

	// Check for updates
	// For Ingress, comparing entire spec can be complex. A common approach is to just replace.
	// More sophisticated would be a 3-way merge patch or strategic merge patch.
	if !reflect.DeepEqual(foundIngress.Spec, desiredIngress.Spec) ||
		!reflect.DeepEqual(foundIngress.Annotations, desiredIngress.Annotations) || // Also check annotations
		!reflect.DeepEqual(foundIngress.Labels, desiredIngress.Labels) { // And labels
		logger.Info("Updating existing Ingress", "Ingress.Namespace", foundIngress.Namespace, "Ingress.Name", foundIngress.Name)
		// Preserve resource version for update
		desiredIngress.ResourceVersion = foundIngress.ResourceVersion
		// Update labels and annotations directly on the object to be updated
		foundIngress.Labels = desiredIngress.Labels
		foundIngress.Annotations = desiredIngress.Annotations
		foundIngress.Spec = desiredIngress.Spec // Replace spec

		err = r.Update(ctx, foundIngress)
		if err != nil {
			return nil, fmt.Errorf("failed to update Ingress: %w", err)
		}
		r.Recorder.Event(app, corev1.EventTypeNormal, ReasonIngressUpdated, fmt.Sprintf("Updated Ingress %s/%s", foundIngress.Namespace, foundIngress.Name))
		return foundIngress, nil
	}

	logger.V(1).Info("Ingress is up-to-date", "Ingress.Namespace", foundIngress.Namespace, "Ingress.Name", foundIngress.Name)
	return foundIngress, nil
}

func (r *ApplicationReconciler) ensureIngressDeleted(ctx context.Context, app *appsv1alpha1.Application) error {
	logger := log.FromContext(ctx)
	ingressName := app.Name + "-ingress"

	ingressToDelete := &networkingv1.Ingress{
		ObjectMeta: metav1.ObjectMeta{Name: ingressName, Namespace: app.Namespace},
	}

	// Use client.IgnoreNotFound for Delete to make it idempotent
	err := r.Delete(ctx, ingressToDelete, client.PropagationPolicy(metav1.DeletePropagationForeground))
	if err != nil && !apierrors.IsNotFound(err) { // Only return actual errors, not NotFound
		return fmt.Errorf("failed to delete Ingress %s: %w", ingressName, err)
	}
	if err == nil || apierrors.IsNotFound(err) { // Successfully deleted or was already gone
		logger.Info("Ensured Ingress is deleted (or was not found)", "IngressName", ingressName)
		if !apierrors.IsNotFound(err) { // Only record event if it was actually deleted now
			r.Recorder.Event(app, corev1.EventTypeNormal, ReasonIngressDeleted, fmt.Sprintf("Ingress %s/%s deleted as spec.ingress is nil", app.Namespace, ingressName))
		}
	}
	return nil
}

func (r *ApplicationReconciler) getAppLabels(app *appsv1alpha1.Application, componentName string) map[string]string {
	labels := make(map[string]string)
	for k, v := range app.Labels { // Start with user-defined labels from CR
		labels[k] = v
	}
	// Add controller-specific labels, potentially overriding user-defined ones if names clash
	labels["app.kubernetes.io/name"] = app.Name
	labels["app.kubernetes.io/instance"] = app.Name
	labels["app.kubernetes.io/managed-by"] = "application-lifecycle-manager" // Controller name
	labels["app.kubernetes.io/component"] = componentName
	return labels
}

func (r *ApplicationReconciler) getSelectorLabels(app *appsv1alpha1.Application) map[string]string {
	return map[string]string{
		"app.kubernetes.io/name":     app.Name,
		"app.kubernetes.io/instance": app.Name,
	}
}

func safeResourceRequirements(reqs *corev1.ResourceRequirements) corev1.ResourceRequirements {
	if reqs == nil {
		return corev1.ResourceRequirements{}
	}
	return *reqs
}

func setApplicationCondition(status *appsv1alpha1.ApplicationStatus, conditionType string, conditionStatus metav1.ConditionStatus, reason, message string, observedGeneration int64) bool {
	now := metav1.Now()
	newCondition := metav1.Condition{
		Type: conditionType, Status: conditionStatus, Reason: reason, Message: message,
		LastTransitionTime: now, ObservedGeneration: observedGeneration,
	}
	if status.Conditions == nil {
		status.Conditions = []metav1.Condition{}
	}
	for i, c := range status.Conditions {
		if c.Type == conditionType {
			if c.Status == newCondition.Status && c.Reason == newCondition.Reason && c.Message == newCondition.Message && c.ObservedGeneration == newCondition.ObservedGeneration {
				return false // No change
			}
			status.Conditions[i] = newCondition
			return true // Updated
		}
	}
	status.Conditions = append(status.Conditions, newCondition)
	return true // Added
}

func updateConditionsFromDeployment(deployment *appsv1.Deployment, status *appsv1alpha1.ApplicationStatus, observedGen int64) bool {
	changed := false
	// Progressing Condition
	// True if all replicas are updated and available according to Deployment status checks.
	// False if rollout is in progress or replicas are unavailable.
	var depProgressingCond *appsv1.DeploymentCondition
	for _, cond := range deployment.Status.Conditions {
		if cond.Type == appsv1.DeploymentProgressing {
			tempCond := cond // Avoid issues with loop variable pointer
			depProgressingCond = &tempCond
			break
		}
	}

	var newProgressingStatus metav1.ConditionStatus
	var newProgressingReason string
	var newProgressingMessage string

	if depProgressingCond != nil && depProgressingCond.Status == corev1.ConditionTrue && depProgressingCond.Reason == "NewReplicaSetAvailable" {
		newProgressingStatus = metav1.ConditionTrue
		newProgressingReason = ReasonDeploymentProgressing
		newProgressingMessage = "Deployment rollout completed and replicas are available."
	} else {
		newProgressingStatus = metav1.ConditionFalse
		newProgressingReason = ReasonDeploymentProgressing
		if depProgressingCond != nil {
			newProgressingMessage = fmt.Sprintf("Deployment is progressing: %s", depProgressingCond.Message)
		} else {
			newProgressingMessage = "Deployment status is not yet indicating progress towards completion."
		}
	}
	if setApplicationCondition(status, ConditionProgressing, newProgressingStatus, newProgressingReason, newProgressingMessage, observedGen) {
		changed = true
	}

	// Available Condition
	// True if current available replicas meet desired replicas.
	var newAvailableStatus metav1.ConditionStatus
	var newAvailableReason string
	var newAvailableMessage string
	if deployment.Status.AvailableReplicas >= *deployment.Spec.Replicas {
		newAvailableStatus = metav1.ConditionTrue
		newAvailableReason = "MinimumReplicasAvailable"
		newAvailableMessage = fmt.Sprintf("Deployment has %d/%d available replicas.", deployment.Status.AvailableReplicas, *deployment.Spec.Replicas)
	} else {
		newAvailableStatus = metav1.ConditionFalse
		newAvailableReason = "ReplicasUnavailable"
		newAvailableMessage = fmt.Sprintf("Deployment has %d/%d available replicas, want %d.", deployment.Status.AvailableReplicas, *deployment.Spec.Replicas, *deployment.Spec.Replicas)
	}
	if setApplicationCondition(status, ConditionAvailable, newAvailableStatus, newAvailableReason, newAvailableMessage, observedGen) {
		changed = true
	}
	return changed
}

func isAppReady(status *appsv1alpha1.ApplicationStatus) bool {
	isAvailable := false
	isProgressingStable := false // Progressing should be True (meaning rollout complete)
	isDegraded := false

	for _, cond := range status.Conditions {
		if cond.Type == ConditionAvailable && cond.Status == metav1.ConditionTrue {
			isAvailable = true
		}
		if cond.Type == ConditionProgressing && cond.Status == metav1.ConditionTrue {
			isProgressingStable = true
		}
		if cond.Type == ConditionDegraded && cond.Status == metav1.ConditionTrue {
			isDegraded = true
		}
	}
	return isAvailable && isProgressingStable && !isDegraded
}

func (r *ApplicationReconciler) SetupWithManager(mgr ctrl.Manager) error {
	r.Recorder = mgr.GetEventRecorderFor("application-controller")
	return ctrl.NewControllerManagedBy(mgr).
		For(&appsv1alpha1.Application{}).
		Owns(&appsv1.Deployment{}).
		Owns(&corev1.Service{}).
		Owns(&networkingv1.Ingress{}).
		Complete(r)
}
