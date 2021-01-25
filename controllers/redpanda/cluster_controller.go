// Copyright 2021 Vectorized, Inc.
//
// Use of this software is governed by the Business Source License
// included in the file licenses/BSL.md
//
// As of the Change Date specified in that file, in accordance with
// the Business Source License, use of this software will be governed
// by the Apache License, Version 2.0

// Package redpanda contains reconciliation logic for redpanda.vectorized.io CRD
package redpanda

import (
	"context"
	"fmt"
	"reflect"

	"github.com/go-logr/logr"
	redpandav1alpha1 "github.com/vectorizedio/redpanda/src/go/k8s/apis/redpanda/v1alpha1"
	"github.com/vectorizedio/redpanda/src/go/k8s/pkg/resources"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

const (
	debugLevel = 2
)

// ClusterReconciler reconciles a Cluster object
type ClusterReconciler struct {
	client.Client
	Log	logr.Logger
	Scheme	*runtime.Scheme
}

//+kubebuilder:rbac:groups=redpanda.vectorized.io,resources=clusters,verbs=get;list;watch;create;update;patch;delete
//+kubebuilder:rbac:groups=redpanda.vectorized.io,resources=clusters/status,verbs=get;update;patch
//+kubebuilder:rbac:groups=redpanda.vectorized.io,resources=clusters/finalizers,verbs=update
//+kubebuilder:rbac:groups=apps,resources=statefulsets,verbs=get;list;watch;create;update;patch;
//+kubebuilder:rbac:groups=core,resources=services,verbs=get;list;watch;create;update;patch;
//+kubebuilder:rbac:groups=core,resources=configmaps,verbs=get;list;watch;create;update;patch;
//+kubebuilder:rbac:groups=core,resources=pods,verbs=get;list;watch;

// Reconcile is part of the main kubernetes reconciliation loop which aims to
// move the current state of the cluster closer to the desired state.
// the Cluster object against the actual cluster state, and then
// perform operations to make the cluster state reflect the state specified by
// the user.
//
// For more details, check Reconcile and its Result here:
// - https://pkg.go.dev/sigs.k8s.io/controller-runtime@v0.7.0/pkg/reconcile
// nolint:funlen // The complexity of Reconcile function will be address in the next version
func (r *ClusterReconciler) Reconcile(
	ctx context.Context, req ctrl.Request,
) (ctrl.Result, error) {
	log := r.Log.WithValues("redpandacluster", req.NamespacedName)

	log.Info(fmt.Sprintf("Starting reconcile loop for %v", req.NamespacedName))
	defer log.Info(fmt.Sprintf("Finished reconcile loop for %v", req.NamespacedName))

	var redpandaCluster redpandav1alpha1.Cluster
	if err := r.Get(ctx, req.NamespacedName, &redpandaCluster); err != nil {
		log.Error(err, "Unable to fetch RedpandaCluster")
		// we'll ignore not-found errors, since they can't be fixed by an immediate
		// requeue (we'll need to wait for a new notification), and we can get them
		// on deleted requests.
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	sts := resources.NewStatefulSet(r.Client, &redpandaCluster, r.Scheme)
	toApply := []resources.OwnedResource{
		resources.NewService(r.Client, &redpandaCluster, r.Scheme),
		resources.NewConfigMap(r.Client, &redpandaCluster, r.Scheme),
		sts,
	}

	for _, res := range toApply {
		err := res.Ensure(ctx)
		if err != nil {
			log.Error(err, "Failed to reconcile resource",
				"Namespace", redpandaCluster.Namespace,
				"Name", redpandaCluster.Name,
				"Kind", res.Kind())
		}
	}

	var observedPods corev1.PodList

	err := r.List(ctx, &observedPods, &client.ListOptions{
		LabelSelector:	labels.SelectorFromSet(redpandaCluster.Labels),
		Namespace:	redpandaCluster.Namespace,
	})
	if err != nil {
		log.Error(err, "Unable to fetch PodList resource")

		return ctrl.Result{}, err
	}

	observedNodes := make([]string, 0, len(observedPods.Items))
	// nolint:gocritic // the copies are necessary for further redpandacluster updates
	for _, item := range observedPods.Items {
		observedNodes = append(observedNodes, item.Name)
	}

	if !reflect.DeepEqual(observedNodes, redpandaCluster.Status.Nodes) {
		redpandaCluster.Status.Nodes = observedNodes
		if err := r.Status().Update(ctx, &redpandaCluster); err != nil {
			log.Error(err, "Failed to update RedpandaClusterStatus")

			return ctrl.Result{}, err
		}
	}

	if sts.LastObservedState != nil {
		return ctrl.Result{}, fmt.Errorf("Expecting to have statefulset LastObservedState set but is null")
	}

	if sts.LastObservedState != nil && !reflect.DeepEqual(sts.LastObservedState.Status.ReadyReplicas, redpandaCluster.Status.Replicas) {
		redpandaCluster.Status.Replicas = sts.LastObservedState.Status.ReadyReplicas
		if err := r.Status().Update(ctx, &redpandaCluster); err != nil {
			log.Error(err, "Failed to update RedpandaClusterStatus")

			return ctrl.Result{}, err
		}
	}

	return ctrl.Result{}, nil
}

// SetupWithManager sets up the controller with the Manager.
func (r *ClusterReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&redpandav1alpha1.Cluster{}).
		Owns(&appsv1.StatefulSet{}).
		Complete(r)
}
