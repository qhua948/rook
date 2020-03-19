/*
Copyright 2016 The Rook Authors. All rights reserved.

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

package object

import (
	"context"
	"fmt"
	"reflect"

	"github.com/coreos/pkg/capnslog"
	"github.com/pkg/errors"
	cephv1 "github.com/rook/rook/pkg/apis/ceph.rook.io/v1"
	"github.com/rook/rook/pkg/clusterd"
	cephclient "github.com/rook/rook/pkg/daemon/ceph/client"
	cephconfig "github.com/rook/rook/pkg/daemon/ceph/config"
	"github.com/rook/rook/pkg/operator/ceph/cluster/mon"
	opconfig "github.com/rook/rook/pkg/operator/ceph/config"
	opcontroller "github.com/rook/rook/pkg/operator/ceph/controller"
	"github.com/rook/rook/pkg/operator/k8sutil"
	corev1 "k8s.io/api/core/v1"
	v1 "k8s.io/api/core/v1"
	kerrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/manager"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
	"sigs.k8s.io/controller-runtime/pkg/source"
)

const (
	controllerName = "ceph-object-controller"
)

var logger = capnslog.NewPackageLogger("github.com/rook/rook", controllerName)

// List of object resources to watch by the controller
var objectsToWatch = []runtime.Object{
	&corev1.Secret{TypeMeta: metav1.TypeMeta{Kind: "Secret", APIVersion: corev1.SchemeGroupVersion.String()}},
	&v1.Service{TypeMeta: metav1.TypeMeta{Kind: "Service", APIVersion: v1.SchemeGroupVersion.String()}},
	// Stop watching for Deployment object until https://github.com/rook/rook/issues/5047 is fully resolved
	// &appsv1.Deployment{TypeMeta: metav1.TypeMeta{Kind: "Deployment", APIVersion: appsv1.SchemeGroupVersion.String()}},
}

var cephObjectStoreKind = reflect.TypeOf(cephv1.CephObjectStore{}).Name()

// Sets the type meta for the controller main object
var controllerTypeMeta = metav1.TypeMeta{
	Kind:       cephObjectStoreKind,
	APIVersion: fmt.Sprintf("%s/%s", cephv1.CustomResourceGroup, cephv1.Version),
}

// ReconcileCephObjectStore reconciles a cephObjectStore object
type ReconcileCephObjectStore struct {
	client          client.Client
	scheme          *runtime.Scheme
	context         *clusterd.Context
	cephClusterSpec *cephv1.ClusterSpec
	clusterInfo     *cephconfig.ClusterInfo
}

// Add creates a new cephObjectStore Controller and adds it to the Manager. The Manager will set fields on the Controller
// and Start it when the Manager is Started.
func Add(mgr manager.Manager, context *clusterd.Context) error {
	return add(mgr, newReconciler(mgr, context))
}

// newReconciler returns a new reconcile.Reconciler
func newReconciler(mgr manager.Manager, context *clusterd.Context) reconcile.Reconciler {
	// Add the cephv1 scheme to the manager scheme so that the controller knows about it
	mgrScheme := mgr.GetScheme()
	cephv1.AddToScheme(mgr.GetScheme())

	return &ReconcileCephObjectStore{
		client:  mgr.GetClient(),
		scheme:  mgrScheme,
		context: context,
	}
}

func add(mgr manager.Manager, r reconcile.Reconciler) error {
	// Create a new controller
	c, err := controller.New(controllerName, mgr, controller.Options{Reconciler: r})
	if err != nil {
		return err
	}

	// Watch for changes on the cephObjectStore CRD object
	err = c.Watch(&source.Kind{Type: &cephv1.CephObjectStore{TypeMeta: controllerTypeMeta}}, &handler.EnqueueRequestForObject{}, opcontroller.WatchControllerPredicate())
	if err != nil {
		return err
	}

	// Watch all other resources
	for _, t := range objectsToWatch {
		err = c.Watch(&source.Kind{Type: t}, &handler.EnqueueRequestForOwner{
			IsController: true,
			OwnerType:    &cephv1.CephObjectStore{},
		}, opcontroller.WatchPredicateForNonCRDObject(&cephv1.CephObjectStore{TypeMeta: controllerTypeMeta}, mgr.GetScheme()))
		if err != nil {
			return err
		}
	}

	return nil
}

// Reconcile reads that state of the cluster for a cephObjectStore object and makes changes based on the state read
// and what is in the cephObjectStore.Spec
// The Controller will requeue the Request to be processed again if the returned error is non-nil or
// Result.Requeue is true, otherwise upon completion it will remove the work from the queue.
func (r *ReconcileCephObjectStore) Reconcile(request reconcile.Request) (reconcile.Result, error) {
	// workaround because the rook logging mechanism is not compatible with the controller-runtime loggin interface
	reconcileResponse, err := r.reconcile(request)
	if err != nil {
		logger.Errorf("failed to reconcile %v", err)
	}

	return reconcileResponse, err
}

func (r *ReconcileCephObjectStore) reconcile(request reconcile.Request) (reconcile.Result, error) {
	// Fetch the cephObjectStore instance
	cephObjectStore := &cephv1.CephObjectStore{}
	err := r.client.Get(context.TODO(), request.NamespacedName, cephObjectStore)
	if err != nil {
		if kerrors.IsNotFound(err) {
			logger.Debug("cephObjectStore resource not found. Ignoring since object must be deleted.")
			return reconcile.Result{}, nil
		}
		// Error reading the object - requeue the request.
		return reconcile.Result{}, errors.Wrap(err, "failed to get cephObjectStore")
	}

	// The CR was just created, initializing status fields
	if cephObjectStore.Status == nil {
		cephObjectStore.Status = &cephv1.Status{}
		cephObjectStore.Status.Phase = k8sutil.Created
		err := opcontroller.UpdateStatus(r.client, cephObjectStore)
		if err != nil {
			return reconcile.Result{}, errors.Wrap(err, "failed to set status")
		}
	}

	// Make sure a CephCluster is present otherwise do nothing
	cephClusterSpec, isReadyToReconcile, cephClusterExists, reconcileResponse := opcontroller.IsReadyToReconcile(r.client, r.context, request.NamespacedName)
	if !isReadyToReconcile {
		// This handles the case where the Ceph Cluster is gone and we want to delete that CR
		// We skip the deleteStore() function since everything is gone already
		//
		// Also, only remove the finalizer if the CephCluster is gone
		// If not, we should wait for it to be ready
		// This handles the case where the operator is not ready to accept Ceph command but the cluster exists
		if !cephObjectStore.GetDeletionTimestamp().IsZero() && !cephClusterExists {
			// Remove finalizer
			err := opcontroller.RemoveFinalizer(r.client, cephObjectStore)
			if err != nil {
				return reconcile.Result{}, errors.Wrap(err, "failed to remove finalizer")
			}

			// Return and do not requeue. Successful deletion.
			return reconcile.Result{}, nil
		}

		logger.Debugf("CephCluster resource not ready in namespace %q, retrying in %q.", request.NamespacedName.Namespace, opcontroller.WaitForRequeueIfCephClusterNotReadyAfter.String())
		return reconcileResponse, nil
	}
	r.cephClusterSpec = &cephClusterSpec

	// Populate clusterInfo
	// Always populate it during each reconcile
	clusterInfo, _, _, err := mon.LoadClusterInfo(r.context, request.NamespacedName.Namespace)
	if err != nil {
		return reconcile.Result{}, errors.Wrap(err, "failed to populate cluster info")
	}
	r.clusterInfo = clusterInfo

	// Populate CephVersion
	daemon := string(opconfig.MonType)
	currentCephVersion, err := cephclient.LeastUptodateDaemonVersion(r.context, r.clusterInfo.Name, daemon)
	if err != nil {
		logger.Errorf("failed to retrieve current ceph %q version. %v", daemon, err)
	}
	r.clusterInfo.CephVersion = currentCephVersion

	// Set a finalizer so we can do cleanup before the object goes away
	err = opcontroller.AddFinalizerIfNotPresent(r.client, cephObjectStore)
	if err != nil {
		return reconcile.Result{}, errors.Wrap(err, "failed to add finalizer")
	}

	// DELETE: the CR was deleted
	if !cephObjectStore.GetDeletionTimestamp().IsZero() {
		logger.Debugf("deleting store %q", cephObjectStore.Name)
		cfg := clusterConfig{context: r.context, store: cephObjectStore}
		err := cfg.deleteStore()
		if err != nil {
			return reconcile.Result{}, errors.Wrapf(err, "failed to delete store %q. ", cephObjectStore.Name)
		}

		// Remove finalizer
		err = opcontroller.RemoveFinalizer(r.client, cephObjectStore)
		if err != nil {
			return reconcile.Result{}, errors.Wrap(err, "failed to remove finalizer")
		}

		// Return and do not requeue. Successful deletion.
		return reconcile.Result{}, nil
	}

	// validate the store settings
	if err := validateStore(r.context, cephObjectStore); err != nil {
		return reconcile.Result{}, errors.Wrapf(err, "invalid object store %q arguments", cephObjectStore.Name)
	}

	// RECONCILE SERVICE
	logger.Debug("reconciling object store service")
	serviceIP, err := r.reconcileService(cephObjectStore)
	if err != nil {
		return r.setFailedStatus(cephObjectStore, "failed to reconcile service", err)
	}

	// RECONCILE POOLS
	logger.Debug("reconciling object store pools")
	objContext := NewContext(r.context, cephObjectStore.Name, cephObjectStore.Namespace)
	metadataPoolToModel := *cephObjectStore.Spec.MetadataPool.ToModel("")
	dataPoolToModel := *cephObjectStore.Spec.DataPool.ToModel("")
	err = createPools(objContext, metadataPoolToModel, dataPoolToModel)
	if err != nil {
		return r.setFailedStatus(cephObjectStore, "failed to create object pools", err)
	}

	// RECONCILE REALM
	logger.Debug("reconciling object store realms")
	err = reconcileRealm(objContext, serviceIP, cephObjectStore.Spec.Gateway.Port)
	if err != nil {
		return r.setFailedStatus(cephObjectStore, "failed to create object store realm", err)
	}

	// CREATE/UPDATE
	logger.Debug("reconciling object store deployments")
	reconcileResponse, err = r.reconcileCreateObjectStore(cephObjectStore)
	if err != nil {
		return r.setFailedStatus(cephObjectStore, "failed to create object store deployments", err)
	}

	// Set Ready status, we are done reconciling
	cephObjectStore.Status.Phase = k8sutil.ReadyStatus
	err = opcontroller.UpdateStatus(r.client, cephObjectStore)
	if err != nil {
		return reconcile.Result{}, errors.Wrap(err, "failed to set status")
	}

	// Return and do not requeue
	logger.Debug("done reconciling")
	return reconcile.Result{}, nil

}

func (r *ReconcileCephObjectStore) reconcileCreateObjectStore(cephObjectStore *cephv1.CephObjectStore) (reconcile.Result, error) {
	if r.cephClusterSpec.External.Enable {
		_, err := opcontroller.ValidateCephVersionsBetweenLocalAndExternalClusters(r.context, cephObjectStore.Namespace, r.clusterInfo.CephVersion)
		if err != nil {
			// This handles the case where the operator is running, the external cluster has been upgraded and a CR creation is called
			// If that's a major version upgrade we fail, if it's a minor version, we continue, it's not ideal but not critical
			return reconcile.Result{}, errors.Wrapf(err, "refusing to run new crd")
		}
	}

	cfg := clusterConfig{
		context:           r.context,
		clusterInfo:       r.clusterInfo,
		store:             cephObjectStore,
		rookVersion:       r.cephClusterSpec.CephVersion.Image,
		clusterSpec:       r.cephClusterSpec,
		DataPathMap:       opconfig.NewStatelessDaemonDataPathMap(opconfig.RgwType, cephObjectStore.Name, cephObjectStore.Namespace, r.cephClusterSpec.DataDirHostPath),
		client:            r.client,
		scheme:            r.scheme,
		skipUpgradeChecks: r.cephClusterSpec.SkipUpgradeChecks,
	}

	err := cfg.createOrUpdateStore()
	if err != nil {
		return reconcile.Result{}, errors.Wrapf(err, "failed to create object store %q", cephObjectStore.Name)
	}

	return reconcile.Result{}, nil
}

func (r *ReconcileCephObjectStore) setFailedStatus(cephObjectStore *cephv1.CephObjectStore, errMessage string, err error) (reconcile.Result, error) {
	cephObjectStore.Status.Phase = k8sutil.ReconcileFailedStatus
	errStatus := opcontroller.UpdateStatus(r.client, cephObjectStore)
	if errStatus != nil {
		logger.Errorf("failed to set status. %v", errStatus)
	}

	return reconcile.Result{}, errors.Wrapf(err, "%s", errMessage)
}
