// Copyright 2022 Redpanda Data, Inc.
//
// Use of this software is governed by the Business Source License
// included in the file licenses/BSL.md
//
// As of the Change Date specified in that file, in accordance with
// the Business Source License, use of this software will be governed
// by the Apache License, Version 2.0

package redpanda

import (
	"context"
	"errors"
	"fmt"

	"github.com/go-logr/logr"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/tools/record"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	redpandav1alpha1 "github.com/redpanda-data/redpanda/src/go/k8s/apis/redpanda/v1alpha1"
	adminutils "github.com/redpanda-data/redpanda/src/go/k8s/pkg/admin"
	consolepkg "github.com/redpanda-data/redpanda/src/go/k8s/pkg/console"
	"github.com/redpanda-data/redpanda/src/go/k8s/pkg/resources"
)

// ConsoleReconciler reconciles a Console object
type ConsoleReconciler struct {
	client.Client
	Scheme                *runtime.Scheme
	Log                   logr.Logger
	AdminAPIClientFactory adminutils.AdminAPIClientFactory
	clusterDomain         string
	Store                 *consolepkg.Store
	EventRecorder         record.EventRecorder
}

const (
	// Warning event if referenced Cluster not found
	ClusterNotFoundEvent = "ClusterNotFound"

	// Warning event if subdomain is not found in Cluster ExternalListener
	NoSubdomainEvent = "NoSubdomain"
)

//+kubebuilder:rbac:groups=apps,resources=deployments,verbs=get;list;watch;create;update;patch;delete
//+kubebuilder:rbac:groups=apps,resources=configmaps,verbs=get;list;watch;create;update;patch;delete
//+kubebuilder:rbac:groups="",resources=events,verbs=get;list;watch;create;update;patch

//+kubebuilder:rbac:groups=redpanda.vectorized.io,resources=consoles,verbs=get;list;watch;create;update;patch;delete
//+kubebuilder:rbac:groups=redpanda.vectorized.io,resources=consoles/status,verbs=get;update;patch
//+kubebuilder:rbac:groups=redpanda.vectorized.io,resources=consoles/finalizers,verbs=update

func (r *ConsoleReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	log := r.Log.WithValues("redpandaconsole", req.NamespacedName)

	log.Info(fmt.Sprintf("Starting reconcile loop for %v", req.NamespacedName))
	defer log.Info(fmt.Sprintf("Finished reconcile loop for %v", req.NamespacedName))

	console := &redpandav1alpha1.Console{}
	if err := r.Get(ctx, req.NamespacedName, console); err != nil {
		if apierrors.IsNotFound(err) {
			return ctrl.Result{}, nil
		}
		return ctrl.Result{}, err
	}

	cluster := &redpandav1alpha1.Cluster{}
	if err := r.Get(ctx, console.GetClusterRef(), cluster); err != nil {
		if apierrors.IsNotFound(err) {
			// Console will never reconcile if Cluster is not found
			// Users shouldn't check logs of operator to know this
			// Adding Conditions in Console status might not be apt, record Event instead
			// Alternatively, we can have this validation via Webhook
			r.EventRecorder.Eventf(
				console,
				corev1.EventTypeWarning, ClusterNotFoundEvent,
				"Unable to reconcile Console as the referenced Cluster %s/%s is not found",
				console.Spec.ClusterKeyRef.Namespace, console.Spec.ClusterKeyRef.Name,
			)
		}
		return ctrl.Result{}, err
	}

	var s state
	switch {
	case console.GetDeletionTimestamp() != nil:
		s = &Deleting{r}
	case !console.GenerationMatchesObserved():
		if err := r.handleSpecChange(ctx, console, log); err != nil {
			return ctrl.Result{}, fmt.Errorf("handle spec change: %w", err)
		}
		fallthrough
	default:
		s = &Reconciling{r}
	}

	return s.Do(ctx, console, cluster, log)
}

// Reconciling is the state of the Console that handles reconciliation
type Reconciling ConsoleState

// Do handles reconciliation of Console
func (r *Reconciling) Do(ctx context.Context, console *redpandav1alpha1.Console, cluster *redpandav1alpha1.Cluster, log logr.Logger) (ctrl.Result, error) {
	// Ensure items in the store are updated
	if err := r.Store.Sync(cluster); err != nil {
		return ctrl.Result{}, fmt.Errorf("sync console store: %w", err)
	}

	// ConfigMap is set to immutable and a new one is created if needed every reconcile
	// Cleanup unused ConfigMaps before ensuring Resources which might create new ConfigMaps again
	// Otherwise, if reconciliation always fail, a lot of unused ConfigMaps will be created
	configmapResource := consolepkg.NewConfigMap(r.Client, r.Scheme, console, cluster, log)
	if err := configmapResource.DeleteUnused(ctx); err != nil {
		return ctrl.Result{}, fmt.Errorf("deleting unused configmaps: %w", err)
	}

	// NewIngress will not create Ingress if subdomain is empty
	subdomain := ""
	if s := cluster.ExternalListener().GetExternal().Subdomain; s != "" {
		subdomain = fmt.Sprintf("console.%s", s)
	} else {
		r.EventRecorder.Event(
			console,
			corev1.EventTypeWarning, NoSubdomainEvent,
			"No Ingress created because no subdomain is found in Cluster ExternalListener",
		)
	}

	applyResources := []resources.Resource{
		consolepkg.NewKafkaSA(r.Client, r.Scheme, console, cluster, r.clusterDomain, r.AdminAPIClientFactory, log),
		consolepkg.NewKafkaACL(r.Client, r.Scheme, console, cluster, log),
		configmapResource,
		consolepkg.NewDeployment(r.Client, r.Scheme, console, cluster, r.Store, log),
		consolepkg.NewService(r.Client, r.Scheme, console, log),
		resources.NewIngress(r.Client, console, r.Scheme, subdomain, console.GetName(), consolepkg.ServicePortName, log).WithTLS(resources.LEClusterIssuer),
	}
	for _, each := range applyResources {
		if err := each.Ensure(ctx); err != nil {
			var ra *resources.RequeueAfterError
			if errors.As(err, &ra) {
				log.V(debugLogLevel).Info(fmt.Sprintf("Requeue ensuring resource after %d: %s", ra.RequeueAfter, ra.Msg))
				// RequeueAfterError is used to delay retry
				log.Info(fmt.Sprintf("Ensuring resource failed, requeueing after %s: %s", ra.RequeueAfter, ra.Msg))
				return ctrl.Result{RequeueAfter: ra.RequeueAfter}, nil
			}
			var r *resources.RequeueError
			if errors.As(err, &r) {
				log.V(debugLogLevel).Info(fmt.Sprintf("Requeue ensuring resource: %s", r.Msg))
				// RequeueError is used to skip controller logging the error and using default retry backoff
				// Don't return the error, as it is most likely not an actual error
				return ctrl.Result{Requeue: true}, nil
			}
			return ctrl.Result{}, err
		}
	}

	if !console.GenerationMatchesObserved() {
		console.Status.ObservedGeneration = console.GetGeneration()
		if err := r.Status().Update(ctx, console); err != nil {
			return ctrl.Result{}, err
		}
	}

	return ctrl.Result{}, nil
}

// Deleting is the state of the Console that handles deletion
type Deleting ConsoleState

// Do handles deletion of Console
func (r *Deleting) Do(ctx context.Context, console *redpandav1alpha1.Console, cluster *redpandav1alpha1.Cluster, log logr.Logger) (ctrl.Result, error) {
	applyResources := []resources.ManagedResource{
		consolepkg.NewKafkaSA(r.Client, r.Scheme, console, cluster, r.clusterDomain, r.AdminAPIClientFactory, log),
		consolepkg.NewKafkaACL(r.Client, r.Scheme, console, cluster, log),
	}

	for _, each := range applyResources {
		if err := each.Cleanup(ctx); err != nil {
			return ctrl.Result{}, err
		}
	}

	return ctrl.Result{}, nil
}

// handleSpecChange is a hook to call before Reconciling
func (r *ConsoleReconciler) handleSpecChange(ctx context.Context, console *redpandav1alpha1.Console, log logr.Logger) error {
	if console.Status.ConfigMapRef != nil {
		console.Status.ConfigMapRef = nil
		if err := r.Status().Update(ctx, console); err != nil {
			return err
		}
	}
	return nil
}

// SetupWithManager sets up the controller with the Manager.
func (r *ConsoleReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&redpandav1alpha1.Console{}).
		Owns(&corev1.Secret{}).
		Owns(&corev1.ServiceAccount{}).
		Owns(&appsv1.Deployment{}).
		Owns(&corev1.Service{}).
		Complete(r)
}

// WithClusterDomain sets the clusterDomain
func (r *ConsoleReconciler) WithClusterDomain(clusterDomain string) *ConsoleReconciler {
	r.clusterDomain = clusterDomain
	return r
}
