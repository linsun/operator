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

	"gopkg.in/yaml.v2"
	"k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/manager"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
	"sigs.k8s.io/controller-runtime/pkg/source"

	"istio.io/operator/pkg/apis/istio/v1alpha2"
	"istio.io/operator/pkg/helmreconciler"
	"istio.io/operator/pkg/util"
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
// TODO(user): Modify this Reconcile function to implement your Controller logic.  This example creates
// a Pod as an example
// Note:
// The Controller will requeue the Request to be processed again if the returned error is non-nil or
// Result.Requeue is true, otherwise upon completion it will remove the work from the queue.
func (r *ReconcileIstioControlPlane) Reconcile(request reconcile.Request) (reconcile.Result, error) {
	log.Info("Reconciling IstioControlPlane")
	// Workaroud for issue: https://github.com/istio/istio/issues/17883
	// Using an unstructured object to get deletionTimestamp and finalizers fields.
	u := &unstructured.Unstructured{}
	u.SetGroupVersionKind(util.IstioOperatorGVK)
	if err := r.client.Get(context.TODO(), request.NamespacedName, u); err != nil {
		if errors.IsNotFound(err) {
			// Request object not found, could have been deleted after reconcile request.
			// Owned objects are automatically garbage collected. For additional cleanup logic use finalizers.
			// Return and don't requeue
			return reconcile.Result{}, nil
		}
		// Error reading the object - requeue the request.
		return reconcile.Result{}, err
	}
	deleted := u.GetDeletionTimestamp() != nil
	finalizers := u.GetFinalizers()
	finalizerIndex := indexOf(finalizers, finalizer)

	// declare read-only icp instance to create the reconciler
	icp := &v1alpha2.IstioControlPlane{}
	icp.SetGroupVersionKind(util.IstioOperatorGVK)
	if err := r.client.Get(context.TODO(), request.NamespacedName, icp); err != nil {
		log.Errorf("error getting IstioControlPlane icp: %s", err)
	}
	os, err := yaml.Marshal(u.Object["spec"])
	if err != nil {
		return reconcile.Result{}, err
	}
	err = util.UnmarshalWithJSONPB(string(os), icp.Spec)
	if err != nil {
		log.Errorf("Cannot unmarshal: %s", err)
		return reconcile.Result{}, err
	}
	log.Infof("Got IstioControlPlaneSpec: \n\n%s\n", string(os))

	if deleted {
		if finalizerIndex < 0 {
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
		finalizers = append(finalizers[:finalizerIndex], finalizers[finalizerIndex+1:]...)
		u.SetFinalizers(finalizers)
		finalizerError := r.client.Update(context.TODO(), u)
		for retryCount := 0; errors.IsConflict(finalizerError) && retryCount < finalizerMaxRetries; retryCount++ {
			// workaround for https://github.com/kubernetes/kubernetes/issues/73098 for k8s < 1.14
			// TODO: make this error message more meaningful.
			log.Info("conflict during finalizer removal, retrying")
			_ = r.client.Get(context.TODO(), request.NamespacedName, u)
			finalizers = u.GetFinalizers()
			finalizerIndex = indexOf(finalizers, finalizer)
			finalizers = append(finalizers[:finalizerIndex], finalizers[finalizerIndex+1:]...)
			u.SetFinalizers(finalizers)
			finalizerError = r.client.Update(context.TODO(), u)
		}
		if finalizerError != nil {
			log.Errorf("error removing finalizer: %s", finalizerError)
			return reconcile.Result{}, finalizerError
		}
		return reconcile.Result{}, err
	} else if finalizerIndex < 0 {
		// TODO: make this error message more meaningful.
		log.Infof("Adding finalizer %v", finalizer)
		finalizers = append(finalizers, finalizer)
		u.SetFinalizers(finalizers)
		err = r.client.Update(context.TODO(), u)
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

func indexOf(l []string, s string) int {
	for i, elem := range l {
		if elem == s {
			return i
		}
	}
	return -1
}
