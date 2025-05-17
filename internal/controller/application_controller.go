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
	applicationFinalizer        = "apps.example.com/finalizer"
	ConditionTypeReady          = "Ready"                      // For the main Ready condition
	ConditionAvailable          = "Available"
	ConditionProgressing        = "Progressing"
	ConditionDegraded           = "Degraded"
	ReasonComponentsReady       = "ComponentsReady"
	ReasonComponentsNotReady    = "ComponentsNotReady"
	ReasonDeploymentCreated     = "DeploymentCreated"
	ReasonDeploymentUpdated     = "DeploymentUpdated"
	ReasonDeploymentProgressing = "DeploymentProgressing"
	ReasonDeploymentRolledOut   = "DeploymentRolledOut"
	ReasonDeploymentFailed      = "DeploymentFailed"
	ReasonServiceCreated        = "ServiceCreated"
	ReasonServiceUpdated        = "ServiceUpdated"
	ReasonServiceError          = "ServiceError"
	ReasonIngressCreated        = "IngressCreated"
	ReasonIngressUpdated        = "IngressUpdated"
	ReasonIngressDeleted        = "IngressDeleted"
	ReasonIngressError          = "IngressError"
)

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

	// Ensure ObservedGeneration is always set based on the CR we are reconciling
	desiredStatus.ObservedGeneration = appCR.Generation

	// Check if desired status is actually different from appCR.Status.
	if reflect.DeepEqual(appCR.Status, *desiredStatus) {
		logger.V(1).Info("Status is unchanged, skipping update.", "application", appCR.Name)
		return ctrl.Result{}, nil
	}

	err := retry.RetryOnConflict(retry.DefaultRetry, func() error {
		// Fetch the latest version of CR to apply status to
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
		return ctrl.Result{}, err
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

	r.applySpecDefaults(app)

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

	// At the beginning of each reconcile that's not a deletion, set ObservedGeneration.
	if currentStatus.ObservedGeneration != app.Generation {
		currentStatus.ObservedGeneration = app.Generation
		statusNeedsUpdate = true 
	}

	// Reconcile Deployment
	deployment, deploymentErr := r.reconcileDeployment(ctx, app)
	if deploymentErr != nil {
		logger.Error(deploymentErr, "Failed to reconcile Deployment")
		errMsg := "Deployment reconciliation failed: " + deploymentErr.Error()
		if setApplicationCondition(currentStatus, ConditionDegraded, metav1.ConditionTrue, ReasonDeploymentFailed, errMsg, app.Generation) { statusNeedsUpdate = true }
		if setApplicationCondition(currentStatus, ConditionAvailable, metav1.ConditionFalse, ReasonDeploymentFailed, errMsg, app.Generation) { statusNeedsUpdate = true }
		if setApplicationCondition(currentStatus, ConditionProgressing, metav1.ConditionFalse, ReasonDeploymentFailed, errMsg, app.Generation) { statusNeedsUpdate = true }
		if currentStatus.DeploymentName != "" { currentStatus.DeploymentName = ""; statusNeedsUpdate = true }
		if currentStatus.AvailableReplicas != 0 { currentStatus.AvailableReplicas = 0; statusNeedsUpdate = true }
	} else if deployment != nil { // Deployment reconciled successfully (created or updated or found up-to-date)
		if currentStatus.DeploymentName != deployment.Name { currentStatus.DeploymentName = deployment.Name; statusNeedsUpdate = true }
		if currentStatus.AvailableReplicas != deployment.Status.AvailableReplicas { currentStatus.AvailableReplicas = deployment.Status.AvailableReplicas; statusNeedsUpdate = true }
		if updateConditionsFromDeployment(deployment, currentStatus, app.Generation) { statusNeedsUpdate = true }
	}

	// Reconcile Service
	service, serviceErr := r.reconcileService(ctx, app)
	if serviceErr != nil {
		logger.Error(serviceErr, "Failed to reconcile Service")
		errMsg := "Service reconciliation failed: " + serviceErr.Error()
		if setApplicationCondition(currentStatus, ConditionDegraded, metav1.ConditionTrue, ReasonServiceError, errMsg, app.Generation) { statusNeedsUpdate = true }
		if currentStatus.ServiceName != "" { currentStatus.ServiceName = ""; statusNeedsUpdate = true }
	} else if service != nil {
		if currentStatus.ServiceName != service.Name { currentStatus.ServiceName = service.Name; statusNeedsUpdate = true }
	}

	// Reconcile Ingress (if specified)
	var ingressErr error
	if app.Spec.Ingress != nil {
		// Service must exist and be healthy (no error during its reconcile) for Ingress
		if serviceErr != nil {
			ingressErr = fmt.Errorf("service reconciliation failed, cannot proceed with Ingress: %w", serviceErr)
			logger.Error(ingressErr, "Prerequisite for Ingress not met")
			if setApplicationCondition(currentStatus, ConditionDegraded, metav1.ConditionTrue, ReasonIngressError, ingressErr.Error(), app.Generation) { statusNeedsUpdate = true }
		} else if service == nil {
			ingressErr = fmt.Errorf("service object is nil, cannot create Ingress for service %s", app.Name+"-service")
			logger.Error(ingressErr, "Prerequisite for Ingress not met")
			if setApplicationCondition(currentStatus, ConditionDegraded, metav1.ConditionTrue, ReasonIngressError, ingressErr.Error(), app.Generation) { statusNeedsUpdate = true }
		} else { 
			var ingress *networkingv1.Ingress
			ingress, ingressErr = r.reconcileIngress(ctx, app, service.Name)
			if ingressErr != nil {
				logger.Error(ingressErr, "Failed to reconcile Ingress")
				errMsg := "Ingress reconciliation failed: " + ingressErr.Error()
				if setApplicationCondition(currentStatus, ConditionDegraded, metav1.ConditionTrue, ReasonIngressError, errMsg, app.Generation) { statusNeedsUpdate = true }
				if currentStatus.IngressName != "" { currentStatus.IngressName = ""; statusNeedsUpdate = true }
				if currentStatus.IngressURL != "" { currentStatus.IngressURL = ""; statusNeedsUpdate = true }
			} else if ingress != nil {
				if currentStatus.IngressName != ingress.Name { currentStatus.IngressName = ingress.Name; statusNeedsUpdate = true }
				var newIngressURL string
				if len(ingress.Spec.Rules) > 0 && ingress.Spec.Rules[0].Host != "" {
					scheme := "http"; if len(ingress.Spec.TLS) > 0 { scheme = "https" }
					path := "/"; if len(ingress.Spec.Rules[0].HTTP.Paths) > 0 { path = ingress.Spec.Rules[0].HTTP.Paths[0].Path }
					newIngressURL = fmt.Sprintf("%s://%s%s", scheme, ingress.Spec.Rules[0].Host, path)
				}
				if currentStatus.IngressURL != newIngressURL { currentStatus.IngressURL = newIngressURL; statusNeedsUpdate = true }
			}
		}
	} else { 
		if err := r.ensureIngressDeleted(ctx, app); err != nil {
			ingressErr = err // Capture error from deletion attempt
			logger.Error(ingressErr, "Failed to ensure Ingress is deleted")
			errMsg := "Failed to delete old ingress: " + ingressErr.Error()
			if setApplicationCondition(currentStatus, ConditionDegraded, metav1.ConditionTrue, ReasonIngressError, errMsg, app.Generation) { statusNeedsUpdate = true }
		}
		if currentStatus.IngressName != "" { currentStatus.IngressName = ""; statusNeedsUpdate = true }
		if currentStatus.IngressURL != "" { currentStatus.IngressURL = ""; statusNeedsUpdate = true }
	}

	// Determine overall application readiness and set the "Ready" condition, so that we can show it on status column
	appIsCurrentlyReady := isAppReady(currentStatus) 

	if appIsCurrentlyReady {
		if setApplicationCondition(currentStatus, ConditionTypeReady, metav1.ConditionTrue, ReasonComponentsReady, "Application is fully provisioned and ready.", app.Generation) { statusNeedsUpdate = true }
		if setApplicationCondition(currentStatus, ConditionAvailable, metav1.ConditionTrue, ReasonComponentsReady, "Application components are available.", app.Generation) {statusNeedsUpdate = true}
		if setApplicationCondition(currentStatus, ConditionProgressing, metav1.ConditionTrue, ReasonComponentsReady, "Application deployment is stable and complete.", app.Generation) {statusNeedsUpdate = true}
		if setApplicationCondition(currentStatus, ConditionDegraded, metav1.ConditionFalse, ReasonComponentsReady, "Application is not degraded.", app.Generation) {statusNeedsUpdate = true}
	} else {
		var notReadyReason string = ReasonComponentsNotReady
		var notReadyMessage string = "Application is not yet ready; see other conditions for details."

		for _, cond := range currentStatus.Conditions {
			if cond.Type == ConditionDegraded && cond.Status == metav1.ConditionTrue {
				notReadyReason = cond.Reason 
				notReadyMessage = fmt.Sprintf("Application is not ready: Degraded - %s", cond.Message)
				break
			}
		}
		if notReadyReason == ReasonComponentsNotReady { 
			for _, cond := range currentStatus.Conditions {
				if cond.Type == ConditionProgressing && cond.Status == metav1.ConditionFalse {
					notReadyReason = cond.Reason 
					notReadyMessage = fmt.Sprintf("Application is not ready: Progressing - %s", cond.Message)
					break
				}
			}
		}
		if notReadyReason == ReasonComponentsNotReady {
			for _, cond := range currentStatus.Conditions {
				if cond.Type == ConditionAvailable && cond.Status == metav1.ConditionFalse {
					notReadyReason = cond.Reason
					notReadyMessage = fmt.Sprintf("Application is not ready: Not Available - %s", cond.Message)
					break
				}
			}
		}
		if setApplicationCondition(currentStatus, ConditionTypeReady, metav1.ConditionFalse, notReadyReason, notReadyMessage, app.Generation) { statusNeedsUpdate = true }
	}

	var finalErr error
	var finalResult ctrl.Result = ctrl.Result{}

	if statusNeedsUpdate {
		logger.Info("Status requires update.", "application", app.Name)
		finalResult, finalErr = r.updateFullStatus(ctx, app, currentStatus)
		if finalErr != nil {
			return finalResult, finalErr 
		}
	} else {
		logger.V(1).Info("No status changes required for this reconciliation cycle.", "application", app.Name)
	}
	
	if deploymentErr != nil { return ctrl.Result{}, deploymentErr }
	if serviceErr != nil { return ctrl.Result{}, serviceErr }
	if ingressErr != nil { return ctrl.Result{}, ingressErr }

	// Requeue if deployment is not yet fully available (replicas not met)
	appIsNowConsideredReady := isAppReady(currentStatus) 

	if !appIsNowConsideredReady {
		requeueDelay := 15 * time.Second
		isStillProgressing := false
		for _, cond := range currentStatus.Conditions {
			if cond.Type == ConditionProgressing && cond.Status == metav1.ConditionFalse {
				isStillProgressing = true
				break
			}
		}
		if (deployment != nil && deployment.Status.AvailableReplicas < *app.Spec.Replicas) || isStillProgressing {
			logger.Info("Application not fully ready or deployment progressing, requeuing.", 
				"desiredReplicas", *app.Spec.Replicas, 
				"availableReplicas", currentStatus.AvailableReplicas, 
				"isStillProgressing", isStillProgressing,
				"requeueAfter", requeueDelay)
		} else {
            requeueDelay = 30 * time.Second 
			logger.Info("Application not fully ready (check component statuses), requeuing.", "requeueAfter", requeueDelay)
        }
		return ctrl.Result{RequeueAfter: requeueDelay}, nil
	}

	return finalResult, finalErr
}

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

	containerPortVal := int32(80)
	if app.Spec.ContainerPort != nil {
		containerPortVal = *app.Spec.ContainerPort
	}

	if app.Spec.Service != nil {
		if app.Spec.Service.Port == nil {
			app.Spec.Service.Port = &containerPortVal
			changed = true
		}
		// Type is always ClusterIP for this controller version, ensure CRD validation/defaulting handles this.
		// If spec.service.type field still exists and can be other values, you'd validate/default here.
		// For now, assuming it's correctly managed to be ClusterIP by CRD or implicitly.
		if app.Spec.Service.Type == nil { // If field exists and is nil
			serviceTypeClusterIP := corev1.ServiceTypeClusterIP
			app.Spec.Service.Type = &serviceTypeClusterIP
			changed = true
		} else if *app.Spec.Service.Type != corev1.ServiceTypeClusterIP {
			log.Log.Info("Warning: Service.Type specified as non-ClusterIP, but controller only supports ClusterIP. Will default to ClusterIP.", "application", app.Name, "specifiedType", *app.Spec.Service.Type)
			serviceTypeClusterIP := corev1.ServiceTypeClusterIP
			app.Spec.Service.Type = &serviceTypeClusterIP
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

// reconcileDeployment creates or updates the Deployment.
func (r *ApplicationReconciler) reconcileDeployment(ctx context.Context, app *appsv1alpha1.Application) (*appsv1.Deployment, error) {
	logger := log.FromContext(ctx)
	deploymentName := app.Name + "-deployment"

	replicas := app.Spec.Replicas             
	containerPort := *app.Spec.ContainerPort 

	desiredDeployment := &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{
			Name:      deploymentName,
			Namespace: app.Namespace,
			Labels:    r.getAppLabels(app, "deployment"),
		},
		Spec: appsv1.DeploymentSpec{
			Replicas: replicas,
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
							ContainerPort: containerPort,
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
			r.Recorder.Eventf(app, corev1.EventTypeWarning, ReasonDeploymentFailed, "Failed to create Deployment %s: %v", desiredDeployment.Name, err)
			return nil, fmt.Errorf("failed to create Deployment: %w", err)
		}
		r.Recorder.Eventf(app, corev1.EventTypeNormal, ReasonDeploymentCreated, "Created Deployment %s/%s", desiredDeployment.Namespace, desiredDeployment.Name)
		return desiredDeployment, nil
	} else if err != nil {
		return nil, fmt.Errorf("failed to get Deployment: %w", err)
	}

	// Deployment exists, reconcile it using RetryOnConflict for updates
	var updatedDeployment *appsv1.Deployment
	updateErr := retry.RetryOnConflict(retry.DefaultRetry, func() error {
		currentFoundDeployment := &appsv1.Deployment{}
		getErr := r.Get(ctx, types.NamespacedName{Name: deploymentName, Namespace: app.Namespace}, currentFoundDeployment)
		if getErr != nil {
			return fmt.Errorf("failed to re-fetch Deployment during update retry: %w", getErr)
		}

		needsUpdate := false
		// Compare and apply changes from desiredDeployment.Spec to currentFoundDeployment.Spec
		if !reflect.DeepEqual(currentFoundDeployment.Spec.Replicas, desiredDeployment.Spec.Replicas) {
			needsUpdate = true; currentFoundDeployment.Spec.Replicas = desiredDeployment.Spec.Replicas
		}
		if !reflect.DeepEqual(currentFoundDeployment.Spec.Template.Spec, desiredDeployment.Spec.Template.Spec) {
			needsUpdate = true
			currentFoundDeployment.Spec.Template.Spec = desiredDeployment.Spec.Template.Spec
		}
		if !reflect.DeepEqual(currentFoundDeployment.Spec.Template.ObjectMeta.Labels, desiredDeployment.Spec.Template.ObjectMeta.Labels) {
			needsUpdate = true
			currentFoundDeployment.Spec.Template.ObjectMeta.Labels = desiredDeployment.Spec.Template.ObjectMeta.Labels
		}


		// Compare top-level labels and annotations of the Deployment itself
		if !reflect.DeepEqual(currentFoundDeployment.Labels, desiredDeployment.Labels) {
			needsUpdate = true; currentFoundDeployment.Labels = desiredDeployment.Labels
		}
		if currentFoundDeployment.Annotations == nil { currentFoundDeployment.Annotations = make(map[string]string) }
		for k, v := range desiredDeployment.Annotations {
			if currentFoundDeployment.Annotations[k] != v {
				currentFoundDeployment.Annotations[k] = v
				needsUpdate = true
			}
		}


		if !needsUpdate {
			logger.V(1).Info("Deployment is already in desired state within retry loop.", "Deployment.Name", deploymentName)
			updatedDeployment = currentFoundDeployment
			return nil
		}

		logger.Info("Attempting to update existing Deployment", "Deployment.Name", deploymentName)
		updateOpErr := r.Update(ctx, currentFoundDeployment)
		if updateOpErr == nil {
			updatedDeployment = currentFoundDeployment
		}
		return updateOpErr
	})

	if updateErr != nil {
		r.Recorder.Eventf(app, corev1.EventTypeWarning, ReasonDeploymentFailed, "Failed to update Deployment %s after retries: %v", deploymentName, updateErr)
		return nil, fmt.Errorf("failed to update Deployment '%s' after retries: %w", deploymentName, updateErr)
	}
	
	if updatedDeployment == nil {
	    updatedDeployment = foundDeployment
	}

	// Check if an update was *attempted and successful* vs. *no update needed*
	// foundDeployment is the state before the retry loop. updatedDeployment is the state after.
	if !reflect.DeepEqual(foundDeployment.Spec, updatedDeployment.Spec) ||
	   !reflect.DeepEqual(foundDeployment.Labels, updatedDeployment.Labels) ||
	   !reflect.DeepEqual(foundDeployment.Annotations, updatedDeployment.Annotations) {
		r.Recorder.Eventf(app, corev1.EventTypeNormal, ReasonDeploymentUpdated, "Updated Deployment %s/%s", updatedDeployment.Namespace, updatedDeployment.Name)
	} else if updateErr == nil { 
		logger.V(1).Info("Deployment confirmed up-to-date", "Deployment.Namespace", updatedDeployment.Namespace, "Deployment.Name", updatedDeployment.Name)
	}
	
	return updatedDeployment, nil
}

// reconcileService ensures the Service for the Application is correctly configured.
func (r *ApplicationReconciler) reconcileService(ctx context.Context, app *appsv1alpha1.Application) (*corev1.Service, error) {
	logger := log.FromContext(ctx)
	serviceName := app.Name + "-service"

	servicePort := *app.Spec.ContainerPort
	if app.Spec.Service != nil && app.Spec.Service.Port != nil {
		servicePort = *app.Spec.Service.Port
	}

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
		errCreate := r.Create(ctx, desiredService)
		if errCreate != nil {
			r.Recorder.Eventf(app, corev1.EventTypeWarning, ReasonServiceError, "Failed to create Service %s: %v", desiredService.Name, errCreate)
			return nil, fmt.Errorf("failed to create Service: %w", errCreate)
		}
		r.Recorder.Eventf(app, corev1.EventTypeNormal, ReasonServiceCreated, "Created Service %s/%s", desiredService.Namespace, desiredService.Name)
		return desiredService, nil
	} else if err != nil {
		return nil, fmt.Errorf("failed to get Service: %w", err)
	}

	// Service Exists, reconcile it
	var updatedService *corev1.Service
	updateErr := retry.RetryOnConflict(retry.DefaultRetry, func() error {
		currentFoundService := &corev1.Service{}
		getErr := r.Get(ctx, types.NamespacedName{Name: serviceName, Namespace: app.Namespace}, currentFoundService)
		if getErr != nil {
			return fmt.Errorf("failed to re-fetch Service '%s' during update retry: %w", serviceName, getErr)
		}

		needsActualUpdate := false
		if currentFoundService.Spec.Type != corev1.ServiceTypeClusterIP {
			currentFoundService.Spec.Type = corev1.ServiceTypeClusterIP; needsActualUpdate = true
		}
		if !reflect.DeepEqual(currentFoundService.Spec.Ports, desiredService.Spec.Ports) {
			currentFoundService.Spec.Ports = desiredService.Spec.Ports; needsActualUpdate = true
		}
		if !reflect.DeepEqual(currentFoundService.Spec.Selector, desiredService.Spec.Selector) {
			currentFoundService.Spec.Selector = desiredService.Spec.Selector; needsActualUpdate = true
		}
		if !reflect.DeepEqual(currentFoundService.Labels, desiredService.Labels) {
			currentFoundService.Labels = desiredService.Labels; needsActualUpdate = true
		}

		if !needsActualUpdate {
			logger.V(1).Info("Service is already in desired state within retry loop.", "Service.Name", serviceName)
			updatedService = currentFoundService
			return nil
		}
		
		existingClusterIP := currentFoundService.Spec.ClusterIP
		currentFoundService.Spec.Type = corev1.ServiceTypeClusterIP 
		if currentFoundService.Spec.Type == corev1.ServiceTypeClusterIP {
			currentFoundService.Spec.ClusterIP = existingClusterIP
		}


		logger.Info("Attempting to update existing Service", "Service.Name", serviceName)
		updateOpErr := r.Update(ctx, currentFoundService)
		if updateOpErr == nil {
			updatedService = currentFoundService
		}
		return updateOpErr
	})

	if updateErr != nil {
		r.Recorder.Eventf(app, corev1.EventTypeWarning, ReasonServiceError, "Failed to update Service %s after retries: %v", serviceName, updateErr)
		return nil, fmt.Errorf("failed to update Service '%s' after retries: %w", serviceName, updateErr)
	}

	if updatedService == nil { updatedService = foundService }

	if !reflect.DeepEqual(foundService.Spec, updatedService.Spec) || !reflect.DeepEqual(foundService.Labels, updatedService.Labels) {
		r.Recorder.Eventf(app, corev1.EventTypeNormal, ReasonServiceUpdated, "Updated Service %s/%s", updatedService.Namespace, updatedService.Name)
	} else if updateErr == nil {
		logger.V(1).Info("Service confirmed up-to-date", "Service.Namespace", updatedService.Namespace, "Service.Name", updatedService.Name)
	}
	
	return updatedService, nil
}


// reconcileIngress ensures the Ingress for the Application is correctly configured.
func (r *ApplicationReconciler) reconcileIngress(ctx context.Context, app *appsv1alpha1.Application, serviceName string) (*networkingv1.Ingress, error) {
	logger := log.FromContext(ctx)
	ingressName := app.Name + "-ingress"
	ingressSpec := app.Spec.Ingress 

	backendServicePort := *app.Spec.ContainerPort
	if app.Spec.Service != nil && app.Spec.Service.Port != nil {
		backendServicePort = *app.Spec.Service.Port
	}

	desiredIngress := &networkingv1.Ingress{
		ObjectMeta: metav1.ObjectMeta{
			Name: ingressName, Namespace: app.Namespace,
			Labels: r.getAppLabels(app, "ingress"), Annotations: ingressSpec.Annotations,
		},
		Spec: networkingv1.IngressSpec{
			IngressClassName: ingressSpec.IngressClassName, TLS: ingressSpec.TLS,
			Rules: []networkingv1.IngressRule{{
				Host: ingressSpec.Host,
				IngressRuleValue: networkingv1.IngressRuleValue{
					HTTP: &networkingv1.HTTPIngressRuleValue{
						Paths: []networkingv1.HTTPIngressPath{{
							Path: ingressSpec.Path, PathType: ingressSpec.PathType,
							Backend: networkingv1.IngressBackend{
								Service: &networkingv1.IngressServiceBackend{
									Name: serviceName, Port: networkingv1.ServiceBackendPort{Number: backendServicePort},
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
		errCreate := r.Create(ctx, desiredIngress)
		if errCreate != nil {
			r.Recorder.Eventf(app, corev1.EventTypeWarning, ReasonIngressError, "Failed to create Ingress %s: %v", desiredIngress.Name, errCreate)
			return nil, fmt.Errorf("failed to create Ingress: %w", errCreate)
		}
		r.Recorder.Eventf(app, corev1.EventTypeNormal, ReasonIngressCreated, "Created Ingress %s/%s", desiredIngress.Namespace, desiredIngress.Name)
		return desiredIngress, nil
	} else if err != nil {
		return nil, fmt.Errorf("failed to get Ingress: %w", err)
	}

	// Ingress exists, reconcile it
	var updatedIngress *networkingv1.Ingress
	updateErr := retry.RetryOnConflict(retry.DefaultRetry, func() error {
		currentFoundIngress := &networkingv1.Ingress{}
		getErr := r.Get(ctx, types.NamespacedName{Name: ingressName, Namespace: app.Namespace}, currentFoundIngress)
		if getErr != nil {
			return fmt.Errorf("failed to re-fetch Ingress '%s' during update retry: %w", ingressName, getErr)
		}

		needsActualUpdate := false
		if !reflect.DeepEqual(currentFoundIngress.Spec, desiredIngress.Spec) {
			currentFoundIngress.Spec = desiredIngress.Spec; needsActualUpdate = true
		}
		if !reflect.DeepEqual(currentFoundIngress.Labels, desiredIngress.Labels) {
			currentFoundIngress.Labels = desiredIngress.Labels; needsActualUpdate = true
		}
		if !reflect.DeepEqual(currentFoundIngress.Annotations, desiredIngress.Annotations) {
			currentFoundIngress.Annotations = desiredIngress.Annotations; needsActualUpdate = true
		}

		if !needsActualUpdate {
			logger.V(1).Info("Ingress is already in desired state within retry loop.", "Ingress.Name", ingressName)
			updatedIngress = currentFoundIngress
			return nil
		}

		logger.Info("Attempting to update existing Ingress", "Ingress.Name", ingressName)
		updateOpErr := r.Update(ctx, currentFoundIngress)
		if updateOpErr == nil {
			updatedIngress = currentFoundIngress
		}
		return updateOpErr
	})
	
	if updateErr != nil {
		r.Recorder.Eventf(app, corev1.EventTypeWarning, ReasonIngressError, "Failed to update Ingress %s after retries: %v", ingressName, updateErr)
		return nil, fmt.Errorf("failed to update Ingress '%s' after retries: %w", ingressName, updateErr)
	}
	if updatedIngress == nil { updatedIngress = foundIngress }

	if !reflect.DeepEqual(foundIngress.Spec, updatedIngress.Spec) || 
	   !reflect.DeepEqual(foundIngress.Labels, updatedIngress.Labels) ||
	   !reflect.DeepEqual(foundIngress.Annotations, updatedIngress.Annotations) {
		r.Recorder.Eventf(app, corev1.EventTypeNormal, ReasonIngressUpdated, "Updated Ingress %s/%s", updatedIngress.Namespace, updatedIngress.Name)
	} else if updateErr == nil {
		logger.V(1).Info("Ingress confirmed up-to-date", "Ingress.Namespace", updatedIngress.Namespace, "Ingress.Name", updatedIngress.Name)
	}

	return updatedIngress, nil
}

func (r *ApplicationReconciler) ensureIngressDeleted(ctx context.Context, app *appsv1alpha1.Application) error {
	logger := log.FromContext(ctx)
	ingressName := app.Name + "-ingress"
	
	ingressToDelete := &networkingv1.Ingress{
		ObjectMeta: metav1.ObjectMeta{ Name: ingressName, Namespace: app.Namespace },
	}
	
	err := r.Delete(ctx, ingressToDelete, client.PropagationPolicy(metav1.DeletePropagationForeground))
	if err != nil && !apierrors.IsNotFound(err) { 
		r.Recorder.Eventf(app, corev1.EventTypeWarning, ReasonIngressError, "Failed to delete Ingress %s: %v", ingressName, err)
		return fmt.Errorf("failed to delete Ingress %s: %w", ingressName, err)
	}
	if err == nil || apierrors.IsNotFound(err) { 
		if !apierrors.IsNotFound(err) { 
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
	if status.Conditions == nil { status.Conditions = []metav1.Condition{} }
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
		if deployment.Status.UpdatedReplicas < desiredReplicas || deployment.Status.Replicas > deployment.Status.UpdatedReplicas {
			newProgressingMessage = fmt.Sprintf("Deployment rollout in progress: updated %d/%d, total %d", deployment.Status.UpdatedReplicas, desiredReplicas, deployment.Status.Replicas)
		} else if deployment.Status.AvailableReplicas < desiredReplicas {
			newProgressingMessage = fmt.Sprintf("Deployment rollout appears complete but waiting for %d available replicas (currently %d).", desiredReplicas, deployment.Status.AvailableReplicas)
		} else { 
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