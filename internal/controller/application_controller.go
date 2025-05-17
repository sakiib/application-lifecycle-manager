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

	appsv1alpha1 "github.com/sakiib/application-lifecycle-manager/api/v1alpha1" // Ensure this is your correct import path
)

const (
	applicationFinalizer        = "apps.example.com/finalizer"
	ConditionTypeReady          = "Ready" // For the main Ready condition
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

	// Ensure ObservedGeneration is set on the desiredStatus before comparison/update
	desiredStatus.ObservedGeneration = appCR.Generation

	// Check if desired status is actually different from appCR.Status.
	if reflect.DeepEqual(appCR.Status, *desiredStatus) {
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

	currentStatus := app.Status.DeepCopy()
	statusNeedsUpdate := false

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
	} else {
		if controllerutil.ContainsFinalizer(app, applicationFinalizer) {
			logger.Info("Application is being deleted, performing cleanup", "name", app.Name)
			controllerutil.RemoveFinalizer(app, applicationFinalizer)
			if err := r.Update(ctx, app); err != nil {
				logger.Error(err, "Failed to remove finalizer")
				return ctrl.Result{}, err
			}
			logger.Info("Finalizer removed", "name", app.Name)
		}
		return ctrl.Result{}, nil
	}

	currentStatus.ObservedGeneration = app.Generation // Set early, update if other status fields change

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
		// No specific "Ready" condition here, as overall readiness depends on all components
	} else {
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
	}
	deploymentErr := err // Store deployment error to return later if it's the primary issue

	// Reconcile Service
	var service *corev1.Service // Declare service to use its name later
	var serviceErr error
	service, serviceErr = r.reconcileService(ctx, app)
	if serviceErr != nil {
		logger.Error(serviceErr, "Failed to reconcile Service")
		errMsg := "Service reconciliation failed: " + serviceErr.Error()
		if setApplicationCondition(currentStatus, ConditionDegraded, metav1.ConditionTrue, ReasonServiceError, errMsg, app.Generation) {
			statusNeedsUpdate = true
		}
		if currentStatus.ServiceName != "" {
			currentStatus.ServiceName = ""
			statusNeedsUpdate = true
		}
	} else if service != nil {
		if currentStatus.ServiceName != service.Name {
			currentStatus.ServiceName = service.Name
			statusNeedsUpdate = true
		}
	}

	// Reconcile Ingress (if specified)
	var ingressErr error
	if app.Spec.Ingress != nil {
		if service == nil && serviceErr == nil { // Service must exist and be healthy for Ingress
			serviceErr = fmt.Errorf("service %s not found or not ready, cannot create Ingress", app.Name+"-service")
			logger.Error(serviceErr, "Prerequisite for Ingress not met")
			if setApplicationCondition(currentStatus, ConditionDegraded, metav1.ConditionTrue, ReasonIngressError, serviceErr.Error(), app.Generation) {
				statusNeedsUpdate = true
			}
		}

		if serviceErr == nil { // Only proceed if service reconciliation was successful
			var ingress *networkingv1.Ingress
			ingress, ingressErr = r.reconcileIngress(ctx, app, service.Name)
			if ingressErr != nil {
				logger.Error(ingressErr, "Failed to reconcile Ingress")
				errMsg := "Ingress reconciliation failed: " + ingressErr.Error()
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
			} else if ingress != nil {
				if currentStatus.IngressName != ingress.Name {
					currentStatus.IngressName = ingress.Name
					statusNeedsUpdate = true
				}
				var newIngressURL string
				if len(ingress.Spec.Rules) > 0 && ingress.Spec.Rules[0].Host != "" {
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
			}
		}
	} else { // Ingress not specified in spec, ensure it's cleaned up
		if err := r.ensureIngressDeleted(ctx, app); err != nil {
			ingressErr = err // Capture error from deletion attempt
			logger.Error(ingressErr, "Failed to ensure Ingress is deleted")
			errMsg := "Failed to delete old ingress: " + ingressErr.Error()
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

	// Set Overall Ready condition based on the individual component conditions
	appIsReady := isAppReady(currentStatus) // isAppReady checks Available, Progressing (True), and Degraded (False)
	if appIsReady {
		if setApplicationCondition(currentStatus, ConditionTypeReady, metav1.ConditionTrue, ReasonComponentsReady, "Application is fully provisioned and ready.", app.Generation) {
			statusNeedsUpdate = true
		}
		// Ensure other top-level summary conditions are consistent
		if setApplicationCondition(currentStatus, ConditionAvailable, metav1.ConditionTrue, ReasonComponentsReady, "Application components are available.", app.Generation) {
			statusNeedsUpdate = true
		}
		if setApplicationCondition(currentStatus, ConditionProgressing, metav1.ConditionTrue, ReasonComponentsReady, "Application deployment is stable and complete.", app.Generation) {
			statusNeedsUpdate = true
		}
		if setApplicationCondition(currentStatus, ConditionDegraded, metav1.ConditionFalse, ReasonComponentsReady, "Application is not degraded.", app.Generation) {
			statusNeedsUpdate = true
		}
	} else {
		// Determine a more specific reason why it's not ready for the Ready=False condition
		var notReadyReason string = ReasonComponentsNotReady
		var notReadyMessage string = "Application is not yet ready."

		// Prioritize Degraded condition message
		for _, cond := range currentStatus.Conditions {
			if cond.Type == ConditionDegraded && cond.Status == metav1.ConditionTrue {
				notReadyReason = cond.Reason
				notReadyMessage = fmt.Sprintf("Application is not ready: Degraded - %s", cond.Message)
				break
			}
		}
		// If not degraded, check if still progressing
		if notReadyReason == ReasonComponentsNotReady { // Only if not already set by Degraded
			for _, cond := range currentStatus.Conditions {
				if cond.Type == ConditionProgressing && cond.Status == metav1.ConditionFalse {
					notReadyReason = cond.Reason
					notReadyMessage = fmt.Sprintf("Application is not ready: Progressing - %s", cond.Message)
					break
				}
			}
		}
		// If not degraded and not progressing=false, check if available is false
		if notReadyReason == ReasonComponentsNotReady {
			for _, cond := range currentStatus.Conditions {
				if cond.Type == ConditionAvailable && cond.Status == metav1.ConditionFalse {
					notReadyReason = cond.Reason
					notReadyMessage = fmt.Sprintf("Application is not ready: Not Available - %s", cond.Message)
					break
				}
			}
		}

		if setApplicationCondition(currentStatus, ConditionTypeReady, metav1.ConditionFalse, notReadyReason, notReadyMessage, app.Generation) {
			statusNeedsUpdate = true
		}
	}

	// Final status update if anything changed
	var finalErr error
	var finalResult ctrl.Result = ctrl.Result{}

	if statusNeedsUpdate || app.Status.ObservedGeneration != currentStatus.ObservedGeneration { // Ensure OG is part of check
		// currentStatus.ObservedGeneration was already set if needed
		logger.Info("Status requires update.", "application", app.Name)
		finalResult, finalErr = r.updateFullStatus(ctx, app, currentStatus)
		if finalErr != nil {
			// If status update fails, that's the error we return for requeue
			return finalResult, finalErr
		}
	} else {
		logger.V(1).Info("No status changes needed for this reconciliation.", "application", app.Name)
	}

	// Determine requeue based on primary errors or deployment state
	if deploymentErr != nil {
		return ctrl.Result{}, deploymentErr
	}
	if serviceErr != nil {
		return ctrl.Result{}, serviceErr
	}
	if ingressErr != nil {
		return ctrl.Result{}, ingressErr
	}

	if deployment != nil && deployment.Status.AvailableReplicas < *app.Spec.Replicas {
		requeueDelay := 15 * time.Second
		logger.Info("Deployment not fully available, requeuing.", "desired", *app.Spec.Replicas, "available", currentStatus.AvailableReplicas, "requeueAfter", requeueDelay)
		return ctrl.Result{RequeueAfter: requeueDelay}, nil
	}
	if !appIsReady { // If app is not ready for other reasons (e.g. progressing but not yet available)
		requeueDelay := 30 * time.Second // Slightly longer general "not ready" requeue
		logger.Info("Application not fully ready (might be progressing or component issue), requeuing.", "requeueAfter", requeueDelay)
		return ctrl.Result{RequeueAfter: requeueDelay}, nil
	}

	return finalResult, finalErr
}

// applySpecDefaults modifies the in-memory app.Spec for processing.
func (r *ApplicationReconciler) applySpecDefaults(app *appsv1alpha1.Application) {
	changed := false
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

	// Ensure ContainerPort is defaulted before using it as a default for Service.Port
	containerPortVal := int32(80) // Default if app.Spec.ContainerPort was initially nil
	if app.Spec.ContainerPort != nil {
		containerPortVal = *app.Spec.ContainerPort
	}

	if app.Spec.Service != nil {
		if app.Spec.Service.Port == nil {
			app.Spec.Service.Port = &containerPortVal // Use the (potentially defaulted) containerPortVal
			changed = true
		}
		if app.Spec.Service.Type == nil { // Ensure this matches the CRD (ClusterIP only)
			defaultServiceType := corev1.ServiceTypeClusterIP
			app.Spec.Service.Type = &defaultServiceType
			changed = true
		}
	} else { // If user wants a service implicitly by defining containerPort, but no service spec
		// This is a design choice: Do we create a Service by default if Ingress is defined or just containerPort?
		// For now, let's assume Service spec must be present to create a service.
		// If you want to default a Service creation:
		// app.Spec.Service = &appsv1alpha1.ApplicationServiceSpec{
		// Port: &containerPortVal,
		// Type: func() *corev1.ServiceType { t := corev1.ServiceTypeClusterIP; return &t }(),
		// }
		// changed = true
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
			Replicas: app.Spec.Replicas,
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
							Name:          "http",
							ContainerPort: *app.Spec.ContainerPort,
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

	// Try to get the existing Deployment
	foundDeployment := &appsv1.Deployment{}
	err := r.Get(ctx, types.NamespacedName{Name: deploymentName, Namespace: app.Namespace}, foundDeployment)

	if err != nil && apierrors.IsNotFound(err) {
		logger.Info("Creating a new Deployment", "Deployment.Namespace", desiredDeployment.Namespace, "Deployment.Name", desiredDeployment.Name)
		err = r.Create(ctx, desiredDeployment)
		if err != nil {
			r.Recorder.Eventf(app, corev1.EventTypeWarning, ReasonDeploymentFailed, "Failed to create Deployment %s: %v", desiredDeployment.Name, err)
			return nil, fmt.Errorf("failed to create Deployment: %w", err)
		}
		r.Recorder.Eventf(app, corev1.EventTypeNormal, ReasonDeploymentCreated, "Created Deployment %s/%s", desiredDeployment.Namespace, desiredDeployment.Name)
		return desiredDeployment, nil // Return the newly created deployment
	} else if err != nil {
		return nil, fmt.Errorf("failed to get Deployment: %w", err)
	}

	// Deployment exists, reconcile it
	// Use RetryOnConflict for the update operation
	var updatedDeployment *appsv1.Deployment
	updateErr := retry.RetryOnConflict(retry.DefaultRetry, func() error {
		// Fetch the latest version of foundDeployment within the retry loop
		// This is crucial to ensure the update is based on the most recent version.
		currentFoundDeployment := &appsv1.Deployment{}
		getErr := r.Get(ctx, types.NamespacedName{Name: deploymentName, Namespace: app.Namespace}, currentFoundDeployment)
		if getErr != nil {
			// If NotFound here, it means it was deleted during retry, which is unusual but possible.
			// Let the outer reconcile handle it or return the error.
			return fmt.Errorf("failed to re-fetch Deployment during update retry: %w", getErr)
		}

		// Compare desired state with currentFoundDeployment and apply changes
		needsUpdate := false
		if !reflect.DeepEqual(currentFoundDeployment.Spec.Replicas, desiredDeployment.Spec.Replicas) {
			needsUpdate = true
			currentFoundDeployment.Spec.Replicas = desiredDeployment.Spec.Replicas
		}
		// Assuming single container for simplicity in this comparison
		if len(currentFoundDeployment.Spec.Template.Spec.Containers) != 1 ||
			len(desiredDeployment.Spec.Template.Spec.Containers) != 1 { // Basic check
			// Handle this case: maybe recreate or error out if container structure is unexpected
			// For now, if container count differs, we might force update with desired spec
			if len(desiredDeployment.Spec.Template.Spec.Containers) == 1 { // Only proceed if desired has one
				needsUpdate = true
				currentFoundDeployment.Spec.Template.Spec.Containers = desiredDeployment.Spec.Template.Spec.Containers
			}
		} else if currentFoundDeployment.Spec.Template.Spec.Containers[0].Image != desiredDeployment.Spec.Template.Spec.Containers[0].Image ||
			!reflect.DeepEqual(currentFoundDeployment.Spec.Template.Spec.Containers[0].Ports, desiredDeployment.Spec.Template.Spec.Containers[0].Ports) ||
			!reflect.DeepEqual(currentFoundDeployment.Spec.Template.Spec.Containers[0].Env, desiredDeployment.Spec.Template.Spec.Containers[0].Env) ||
			!reflect.DeepEqual(currentFoundDeployment.Spec.Template.Spec.Containers[0].Resources, desiredDeployment.Spec.Template.Spec.Containers[0].Resources) ||
			!reflect.DeepEqual(currentFoundDeployment.Spec.Template.Spec.Containers[0].LivenessProbe, desiredDeployment.Spec.Template.Spec.Containers[0].LivenessProbe) ||
			!reflect.DeepEqual(currentFoundDeployment.Spec.Template.Spec.Containers[0].ReadinessProbe, desiredDeployment.Spec.Template.Spec.Containers[0].ReadinessProbe) {
			needsUpdate = true
			currentFoundDeployment.Spec.Template.Spec.Containers = desiredDeployment.Spec.Template.Spec.Containers
		}

		if !reflect.DeepEqual(currentFoundDeployment.Labels, desiredDeployment.Labels) {
			needsUpdate = true
			currentFoundDeployment.Labels = desiredDeployment.Labels
		}
		// For annotations, be careful about overwriting system-added ones.
		// Merge strategy might be better if you only control a subset of annotations.
		if !reflect.DeepEqual(currentFoundDeployment.Annotations, desiredDeployment.Annotations) {
			needsUpdate = true
			currentFoundDeployment.Annotations = desiredDeployment.Annotations
		}

		if !needsUpdate {
			logger.V(1).Info("Deployment is already in desired state, no update needed within retry loop.", "Deployment.Name", deploymentName)
			updatedDeployment = currentFoundDeployment // It's current and up-to-date
			return nil                                 // No update attempt needed
		}

		logger.Info("Attempting to update existing Deployment", "Deployment.Name", deploymentName)
		// The Update call uses currentFoundDeployment which has the latest ResourceVersion
		updateOpErr := r.Update(ctx, currentFoundDeployment)
		if updateOpErr == nil {
			updatedDeployment = currentFoundDeployment // Store the successfully updated object
		}
		return updateOpErr // This error will be checked by RetryOnConflict
	})

	if updateErr != nil {
		r.Recorder.Eventf(app, corev1.EventTypeWarning, ReasonDeploymentFailed, "Failed to update Deployment %s after retries: %v", deploymentName, updateErr)
		return nil, fmt.Errorf("failed to update Deployment after retries: %w", updateErr)
	}

	if updatedDeployment == nil && !apierrors.IsNotFound(err) { // If updateErr was nil but updatedDeployment wasn't set (e.g., needsUpdate was false)
		updatedDeployment = foundDeployment // Use the initially found one if no update occurred.
	}

	if updateErr == nil { // Only record event if update was successful or no update was needed
		// Check if an update actually happened to differentiate event reason
		if !reflect.DeepEqual(foundDeployment.Spec, updatedDeployment.Spec) ||
			!reflect.DeepEqual(foundDeployment.Labels, updatedDeployment.Labels) ||
			!reflect.DeepEqual(foundDeployment.Annotations, updatedDeployment.Annotations) {
			r.Recorder.Eventf(app, corev1.EventTypeNormal, ReasonDeploymentUpdated, "Updated Deployment %s/%s", updatedDeployment.Namespace, updatedDeployment.Name)
		} else {
			logger.V(1).Info("Deployment is up-to-date", "Deployment.Namespace", updatedDeployment.Namespace, "Deployment.Name", updatedDeployment.Name)
		}
	}

	return updatedDeployment, nil
}

func (r *ApplicationReconciler) reconcileService(ctx context.Context, app *appsv1alpha1.Application) (*corev1.Service, error) {
	logger := log.FromContext(ctx)
	serviceName := app.Name + "-service"

	servicePort := *app.Spec.ContainerPort // Defaulted
	if app.Spec.Service != nil && app.Spec.Service.Port != nil {
		servicePort = *app.Spec.Service.Port
	}
	// ServiceType is now always ClusterIP as per earlier discussion
	// If Spec.Service.Type still exists in CRD, it should be defaulted/validated to ClusterIP

	desiredService := &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{
			Name: serviceName, Namespace: app.Namespace,
			Labels: r.getAppLabels(app, "service"),
		},
		Spec: corev1.ServiceSpec{
			Ports: []corev1.ServicePort{{
				Name: "http", Port: servicePort,
				TargetPort: intstr.FromInt32(*app.Spec.ContainerPort), Protocol: corev1.ProtocolTCP,
			}},
			Selector: r.getSelectorLabels(app), Type: corev1.ServiceTypeClusterIP,
		},
	}
	if err := controllerutil.SetControllerReference(app, desiredService, r.Scheme); err != nil {
		return nil, fmt.Errorf("failed to set owner reference on Service: %w", err)
	}

	foundService := &corev1.Service{}
	err := r.Get(ctx, types.NamespacedName{Name: serviceName, Namespace: app.Namespace}, foundService)
	if err != nil && apierrors.IsNotFound(err) {
		logger.Info("Creating a new Service", "Service.Namespace", desiredService.Namespace, "Service.Name", desiredService.Name)
		err = r.Create(ctx, desiredService)
		if err != nil {
			r.Recorder.Eventf(app, corev1.EventTypeWarning, ReasonServiceError, "Failed to create Service %s: %v", desiredService.Name, err)
			return nil, fmt.Errorf("failed to create Service: %w", err)
		}
		r.Recorder.Eventf(app, corev1.EventTypeNormal, ReasonServiceCreated, "Created Service %s/%s", desiredService.Namespace, desiredService.Name)
		return desiredService, nil
	} else if err != nil {
		return nil, fmt.Errorf("failed to get Service: %w", err)
	}

	needsUpdate := false
	if foundService.Spec.Type != corev1.ServiceTypeClusterIP { // Ensure it's ClusterIP
		foundService.Spec.Type = corev1.ServiceTypeClusterIP
		needsUpdate = true
	}
	if len(foundService.Spec.Ports) != 1 ||
		foundService.Spec.Ports[0].Port != servicePort ||
		foundService.Spec.Ports[0].TargetPort.IntVal != *app.Spec.ContainerPort ||
		foundService.Spec.Ports[0].Name != "http" { // Assuming port name is "http"
		foundService.Spec.Ports = desiredService.Spec.Ports
		needsUpdate = true
	}
	if !reflect.DeepEqual(foundService.Spec.Selector, desiredService.Spec.Selector) {
		foundService.Spec.Selector = desiredService.Spec.Selector
		needsUpdate = true
	}
	if !reflect.DeepEqual(foundService.Labels, desiredService.Labels) {
		foundService.Labels = desiredService.Labels
		needsUpdate = true
	}

	if needsUpdate {
		logger.Info("Updating existing Service", "Service.Namespace", foundService.Namespace, "Service.Name", foundService.Name)
		// Preserve ClusterIP if already allocated and type is ClusterIP
		existingClusterIP := foundService.Spec.ClusterIP
		foundService.Spec.Type = corev1.ServiceTypeClusterIP // Explicitly set type
		if foundService.Spec.Type == corev1.ServiceTypeClusterIP {
			foundService.Spec.ClusterIP = existingClusterIP // Preserve
		}

		err = r.Update(ctx, foundService)
		if err != nil {
			r.Recorder.Eventf(app, corev1.EventTypeWarning, ReasonServiceError, "Failed to update Service %s: %v", foundService.Name, err)
			return nil, fmt.Errorf("failed to update Service: %w", err)
		}
		r.Recorder.Eventf(app, corev1.EventTypeNormal, ReasonServiceUpdated, "Updated Service %s/%s", foundService.Namespace, foundService.Name)
		return foundService, nil
	}

	logger.V(1).Info("Service is up-to-date", "Service.Namespace", foundService.Namespace, "Service.Name", foundService.Name)
	return foundService, nil
}

func (r *ApplicationReconciler) reconcileIngress(ctx context.Context, app *appsv1alpha1.Application, serviceName string) (*networkingv1.Ingress, error) {
	logger := log.FromContext(ctx)
	ingressName := app.Name + "-ingress"
	ingressSpec := app.Spec.Ingress // Assumes applySpecDefaults has run

	// Default service port to use for Ingress backend (must be the Service's exposed port)
	backendServicePort := *app.Spec.ContainerPort // Default to container port
	if app.Spec.Service != nil && app.Spec.Service.Port != nil {
		backendServicePort = *app.Spec.Service.Port
	}

	desiredIngress := &networkingv1.Ingress{
		ObjectMeta: metav1.ObjectMeta{
			Name: ingressName, Namespace: app.Namespace,
			Labels:      r.getAppLabels(app, "ingress"),
			Annotations: ingressSpec.Annotations, // from defaulted spec
		},
		Spec: networkingv1.IngressSpec{
			IngressClassName: ingressSpec.IngressClassName, // from defaulted spec
			TLS:              ingressSpec.TLS,              // from spec
			Rules: []networkingv1.IngressRule{{
				Host: ingressSpec.Host,
				IngressRuleValue: networkingv1.IngressRuleValue{
					HTTP: &networkingv1.HTTPIngressRuleValue{
						Paths: []networkingv1.HTTPIngressPath{{
							Path:     ingressSpec.Path,     // from defaulted spec
							PathType: ingressSpec.PathType, // from defaulted spec
							Backend: networkingv1.IngressBackend{
								Service: &networkingv1.IngressServiceBackend{
									Name: serviceName, // Name of the K8s Service
									Port: networkingv1.ServiceBackendPort{Number: backendServicePort},
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
			r.Recorder.Eventf(app, corev1.EventTypeWarning, ReasonIngressError, "Failed to create Ingress %s: %v", desiredIngress.Name, err)
			return nil, fmt.Errorf("failed to create Ingress: %w", err)
		}
		r.Recorder.Eventf(app, corev1.EventTypeNormal, ReasonIngressCreated, "Created Ingress %s/%s", desiredIngress.Namespace, desiredIngress.Name)
		return desiredIngress, nil
	} else if err != nil {
		return nil, fmt.Errorf("failed to get Ingress: %w", err)
	}

	// Compare and update if necessary. Using reflect.DeepEqual for spec can be brittle due to defaultings by K8s.
	// A more robust way is controllerutil.CreateOrPatch or manually comparing fields you control.
	needsUpdate := false
	if !reflect.DeepEqual(foundIngress.Spec.Rules, desiredIngress.Spec.Rules) {
		needsUpdate = true
	}
	if !reflect.DeepEqual(foundIngress.Spec.TLS, desiredIngress.Spec.TLS) {
		needsUpdate = true
	}
	if !reflect.DeepEqual(foundIngress.Spec.IngressClassName, desiredIngress.Spec.IngressClassName) {
		needsUpdate = true
	}
	if !reflect.DeepEqual(foundIngress.Annotations, desiredIngress.Annotations) {
		needsUpdate = true
	} // User-managed annotations
	if !reflect.DeepEqual(foundIngress.Labels, desiredIngress.Labels) {
		needsUpdate = true
	} // User-managed labels

	if needsUpdate {
		logger.Info("Updating existing Ingress", "Ingress.Namespace", foundIngress.Namespace, "Ingress.Name", foundIngress.Name)
		// Preserve immutable or server-set fields if necessary, or fields not managed by this controller.
		// For many Ingress fields, replacing the Spec is common if this controller owns all aspects.
		foundIngress.Spec = desiredIngress.Spec
		foundIngress.Labels = desiredIngress.Labels
		foundIngress.Annotations = desiredIngress.Annotations // This will overwrite any other annotations.
		// If you need to merge, more complex logic is needed.

		err = r.Update(ctx, foundIngress)
		if err != nil {
			r.Recorder.Eventf(app, corev1.EventTypeWarning, ReasonIngressError, "Failed to update Ingress %s: %v", foundIngress.Name, err)
			return nil, fmt.Errorf("failed to update Ingress: %w", err)
		}
		r.Recorder.Eventf(app, corev1.EventTypeNormal, ReasonIngressUpdated, "Updated Ingress %s/%s", foundIngress.Namespace, foundIngress.Name)
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

	err := r.Delete(ctx, ingressToDelete, client.PropagationPolicy(metav1.DeletePropagationForeground))
	// client.IgnoreNotFound will make Delete return nil if the object is already gone.
	if err != nil && !apierrors.IsNotFound(err) {
		r.Recorder.Eventf(app, corev1.EventTypeWarning, ReasonIngressError, "Failed to delete Ingress %s: %v", ingressName, err)
		return fmt.Errorf("failed to delete Ingress %s: %w", ingressName, err)
	}
	if err == nil || apierrors.IsNotFound(err) {
		if !apierrors.IsNotFound(err) { // Only record event if it was actually found and deleted now
			logger.Info("Ingress deleted successfully or was already gone", "IngressName", ingressName)
			r.Recorder.Eventf(app, corev1.EventTypeNormal, ReasonIngressDeleted, "Ingress %s/%s deleted as spec.ingress is nil", app.Namespace, ingressName)
		} else {
			logger.Info("Ingress was not found (already deleted or never created)", "IngressName", ingressName)
		}
	}
	return nil
}

func (r *ApplicationReconciler) getAppLabels(app *appsv1alpha1.Application, componentName string) map[string]string {
	labels := make(map[string]string)
	for k, v := range app.Labels {
		labels[k] = v
	}
	labels["app.kubernetes.io/name"] = app.Name
	labels["app.kubernetes.io/instance"] = app.Name
	labels["app.kubernetes.io/managed-by"] = "application-lifecycle-manager"
	labels["app.kubernetes.io/component"] = componentName
	return labels
}

func (r *ApplicationReconciler) getSelectorLabels(app *appsv1alpha1.Application) map[string]string {
	return map[string]string{
		"app.kubernetes.io/name":     app.Name,
		"app.kubernetes.io/instance": app.Name, // Using app.Name for instance specific selection
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
				return false
			}
			status.Conditions[i] = newCondition
			return true
		}
	}
	status.Conditions = append(status.Conditions, newCondition)
	return true
}

func updateConditionsFromDeployment(deployment *appsv1.Deployment, status *appsv1alpha1.ApplicationStatus, observedGen int64) bool {
	changed := false
	var desiredReplicas int32
	if deployment.Spec.Replicas != nil {
		desiredReplicas = *deployment.Spec.Replicas
	}

	// Available Condition
	newAvailableStatus := metav1.ConditionFalse
	newAvailableReason := ReasonComponentsNotReady
	newAvailableMessage := fmt.Sprintf("Deployment has %d/%d available replicas.", deployment.Status.AvailableReplicas, desiredReplicas)
	if deployment.Status.AvailableReplicas >= desiredReplicas {
		newAvailableStatus = metav1.ConditionTrue
		newAvailableReason = "MinimumReplicasAvailable"
	}
	if setApplicationCondition(status, ConditionAvailable, newAvailableStatus, newAvailableReason, newAvailableMessage, observedGen) {
		changed = true
	}

	// Progressing Condition
	// Check DeploymentProgressing condition from the deployment itself
	var depProgressingCond *appsv1.DeploymentCondition
	for i := range deployment.Status.Conditions {
		if deployment.Status.Conditions[i].Type == appsv1.DeploymentProgressing {
			depProgressingCond = &deployment.Status.Conditions[i]
			break
		}
	}

	newProgressingStatus := metav1.ConditionFalse
	newProgressingReason := ReasonDeploymentProgressing
	newProgressingMessage := "Deployment is progressing."

	if depProgressingCond != nil {
		if depProgressingCond.Status == corev1.ConditionTrue && depProgressingCond.Reason == "NewReplicaSetAvailable" {
			newProgressingStatus = metav1.ConditionTrue
			newProgressingMessage = "Deployment rollout completed and new replica set is available."
		} else {
			newProgressingMessage = fmt.Sprintf("Deployment progressing: %s", depProgressingCond.Message)
		}
	} else {
		// If no Progressing condition from deployment, infer based on replica counts
		if deployment.Status.UpdatedReplicas < desiredReplicas || deployment.Status.Replicas > deployment.Status.UpdatedReplicas {
			newProgressingMessage = fmt.Sprintf("Deployment rollout in progress: updated %d/%d, total %d", deployment.Status.UpdatedReplicas, desiredReplicas, deployment.Status.Replicas)
		} else if deployment.Status.AvailableReplicas < desiredReplicas {
			newProgressingMessage = fmt.Sprintf("Deployment rollout appears complete but waiting for %d available replicas (currently %d).", desiredReplicas, deployment.Status.AvailableReplicas)
		} else { // Updated and Available counts match desired
			newProgressingStatus = metav1.ConditionTrue
			newProgressingMessage = "Deployment rollout appears complete and all replicas available."
		}
	}

	if setApplicationCondition(status, ConditionProgressing, newProgressingStatus, newProgressingReason, newProgressingMessage, observedGen) {
		changed = true
	}
	return changed
}

func isAppReady(status *appsv1alpha1.ApplicationStatus) bool {
	isAvailable := false
	isProgressingStable := false
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
