// Copyright 2019 Istio Authors
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package istiocontrolplane

import (
	"context"

	"k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/util/sets"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/manager"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
	"sigs.k8s.io/controller-runtime/pkg/source"

	"istio.io/operator/pkg/apis/istio/v1alpha2"
	"istio.io/operator/pkg/helmreconciler"
	"istio.io/pkg/log"
)

const (
	finalizer = "istio-finalizer.install.istio.io"
	// finalizerMaxRetries defines the maximum number of attempts to add finalizers.
	finalizerMaxRetries = 10
)

/**
* USER ACTION REQUIRED: This is a scaffold file intended for the user to modify with their own Controller
* business logic.  Delete these comments after modifying this file.*
 */

// Add creates a new IstioControlPlane Controller and adds it to the Manager. The Manager will set fields on the Controller
// and Start it when the Manager is Started.
func Add(mgr manager.Manager) error {
	return add(mgr, newReconciler(mgr))
}

// newReconciler returns a new reconcile.Reconciler
func newReconciler(mgr manager.Manager) reconcile.Reconciler {
	factory := &helmreconciler.Factory{CustomizerFactory: &IstioRenderingCustomizerFactory{}}
	return &ReconcileIstioControlPlane{client: mgr.GetClient(), scheme: mgr.GetScheme(), factory: factory}
}

// add adds a new Controller to mgr with r as the reconcile.Reconciler
func add(mgr manager.Manager, r reconcile.Reconciler) error {
	log.Info("Adding controller for IstioControlPlane")
	// Create a new controller
	c, err := controller.New("istiocontrolplane-controller", mgr, controller.Options{Reconciler: r})
	if err != nil {
		return err
	}

	// Watch for changes to primary resource IstioControlPlane
	err = c.Watch(&source.Kind{Type: &v1alpha2.IstioControlPlane{}}, &handler.EnqueueRequestForObject{})
	if err != nil {
		return err
	}

	log.Info("Controller added")
	return nil
}

var _ reconcile.Reconciler = &ReconcileIstioControlPlane{}

// ReconcileIstioControlPlane reconciles a IstioControlPlane object
type ReconcileIstioControlPlane struct {
	// This client, initialized using mgr.Client() above, is a split client
	// that reads objects from the cache and writes to the apiserver
	client  client.Client
	scheme  *runtime.Scheme
	factory *helmreconciler.Factory
}

// Reconcile reads that state of the cluster for a IstioControlPlane object and makes changes based on the state read
// and what is in the IstioControlPlane.Spec
// Note:
// The Controller will requeue the Request to be processed again if the returned error is non-nil or
// Result.Requeue is true, otherwise upon completion it will remove the work from the queue.
func (r *ReconcileIstioControlPlane) Reconcile(request reconcile.Request) (reconcile.Result, error) {
	log.Info("Reconciling IstioControlPlane")

	// declare read-only icp instance to create the reconciler
	icp := &v1alpha2.IstioControlPlane{}
	if err := r.client.Get(context.TODO(), request.NamespacedName, icp); err != nil {
		if errors.IsNotFound(err) {
			// Request object not found, could have been deleted after reconcile request.
			// Owned objects are automatically garbage collected. For additional cleanup logic use finalizers.
			// Return and don't requeue
			return reconcile.Result{}, nil
		}
		// Error reading the object - requeue the request.
		log.Errorf("error getting IstioControlPlane icp: %s", err)
		return reconcile.Result{}, err
	}

	deleted := icp.GetDeletionTimestamp() != nil
	finalizers := sets.NewString(icp.GetFinalizers()...)
	if deleted {
		if !finalizers.Has(finalizer) {
			log.Info("IstioControlPlane deleted")
			return reconcile.Result{}, nil
		}
		log.Info("Deleting IstioControlPlane")

		reconciler, err := r.factory.New(icp, r.client)
		if err == nil {
			err = reconciler.Delete()
		} else {
			log.Errorf("failed to create reconciler: %s", err)
		}
		// TODO: for now, nuke the resources, regardless of errors
		finalizers.Delete(finalizer)
		icp.SetFinalizers(finalizers.List())
		finalizerError := r.client.Update(context.TODO(), icp)
		for retryCount := 0; errors.IsConflict(finalizerError) && retryCount < finalizerMaxRetries; retryCount++ {
			// workaround for https://github.com/kubernetes/kubernetes/issues/73098 for k8s < 1.14
			// TODO: make this error message more meaningful.
			log.Info("conflict during finalizer removal, retrying")
			_ = r.client.Get(context.TODO(), request.NamespacedName, icp)
			finalizers = sets.NewString(icp.GetFinalizers()...)
			finalizers.Delete(finalizer)
			icp.SetFinalizers(finalizers.List())
			finalizerError = r.client.Update(context.TODO(), icp)
		}
		if finalizerError != nil {
			log.Errorf("error removing finalizer: %s", finalizerError)
			return reconcile.Result{}, finalizerError
		}
		return reconcile.Result{}, err
	} else if !finalizers.Has(finalizer) {
		log.Infof("Adding finalizer %v to %v", finalizer, request)
		finalizers.Insert(finalizer)
		icp.SetFinalizers(finalizers.List())
		err := r.client.Update(context.TODO(), icp)
		if err != nil {
			log.Errorf("Failed to update IstioControlPlane with finalizer, %v", err)
			return reconcile.Result{}, err
		}
	}

	log.Info("Updating IstioControlPlane")
	reconciler, err := r.factory.New(icp, r.client)
	if err == nil {
		err = reconciler.Reconcile()
		if err != nil {
			log.Errorf("reconciling err: %s", err)
		}
	} else {
		log.Errorf("failed to create reconciler: %s", err)
	}

	return reconcile.Result{}, err
}
