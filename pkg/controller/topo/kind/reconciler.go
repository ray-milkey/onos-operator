// Copyright 2019-present Open Networking Foundation.
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

package kind

import (
	"context"
	"github.com/onosproject/onos-api/go/onos/topo"
	"github.com/onosproject/onos-lib-go/pkg/errors"
	"github.com/onosproject/onos-lib-go/pkg/logging"
	"github.com/onosproject/onos-operator/pkg/apis/topo/v1beta1"
	"github.com/onosproject/onos-operator/pkg/controller/util/grpc"
	"github.com/onosproject/onos-operator/pkg/controller/util/k8s"
	"google.golang.org/grpc/status"
	k8serrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/rest"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/manager"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
	"sigs.k8s.io/controller-runtime/pkg/source"
)

var log = logging.GetLogger("controller", "topo", "kind")

const topoService = "onos-topo"
const topoFinalizer = "topo"

// Add creates a new Kind controller and adds it to the Manager. The Manager will set fields on the
// controller and Start it when the Manager is Started.
func Add(mgr manager.Manager) error {
	r := &Reconciler{
		client: mgr.GetClient(),
		scheme: mgr.GetScheme(),
		config: mgr.GetConfig(),
	}

	// Create a new controller
	c, err := controller.New("topo-kind-controller", mgr, controller.Options{Reconciler: r})
	if err != nil {
		return err
	}

	// Watch for changes to primary resource Kind
	err = c.Watch(&source.Kind{Type: &v1beta1.Kind{}}, &handler.EnqueueRequestForObject{})
	if err != nil {
		return err
	}

	// Watch for changes to secondary resource Entity and requeue the Kind
	err = c.Watch(&source.Kind{Type: &v1beta1.Entity{}}, &handler.EnqueueRequestForOwner{
		OwnerType: &v1beta1.Kind{},
	})
	if err != nil {
		return err
	}

	// Watch for changes to secondary resource Relation and requeue the Kind
	err = c.Watch(&source.Kind{Type: &v1beta1.Relation{}}, &handler.EnqueueRequestForOwner{
		OwnerType: &v1beta1.Kind{},
	})
	if err != nil {
		return err
	}

	return nil
}

var _ reconcile.Reconciler = &Reconciler{}

// Reconciler reconciles a Kind object
type Reconciler struct {
	client client.Client
	scheme *runtime.Scheme
	config *rest.Config
}

// Reconcile reads that state of the cluster for a Kind object and makes changes based on the state read
// and what is in the Kind.Spec
func (r *Reconciler) Reconcile(request reconcile.Request) (reconcile.Result, error) {
	log.Infof("Reconciling Kind %s/%s", request.Namespace, request.Name)

	// Fetch the Kind instance
	kind := &v1beta1.Kind{}
	err := r.client.Get(context.TODO(), request.NamespacedName, kind)
	if err != nil {
		if k8serrors.IsNotFound(err) {
			// Request object not found, could have been deleted after reconcile request.
			// Owned objects are automatically garbage collected. For additional cleanup logic use finalizers.
			// Return and don't requeue
			return reconcile.Result{}, nil
		}
		// Error reading the object - requeue the request.
		return reconcile.Result{}, err
	}

	if kind.DeletionTimestamp == nil {
		return r.reconcileCreate(kind)
	} else {
		return r.reconcileDelete(kind)
	}
}

func (r *Reconciler) reconcileCreate(kind *v1beta1.Kind) (reconcile.Result, error) {
	// Add the finalizer to the kind if necessary
	if !k8s.HasFinalizer(kind, topoFinalizer) {
		k8s.AddFinalizer(kind, topoFinalizer)
		err := r.client.Update(context.TODO(), kind)
		if err != nil {
			return reconcile.Result{}, err
		}
	}

	// Connect to the topology service
	conn, err := grpc.ConnectService(r.client, kind.Namespace, topoService)
	if err != nil {
		return reconcile.Result{}, err
	}
	defer conn.Close()

	client := topo.NewTopoClient(conn)

	// Check if the kind exists in the topology and exit reconciliation if so
	if exists, err := r.kindExists(kind, client); err != nil {
		return reconcile.Result{}, err
	} else if exists {
		return reconcile.Result{}, nil
	}

	// If the kind does not exist, create it
	if err := r.createKind(kind, client); err != nil {
		return reconcile.Result{}, err
	}
	return reconcile.Result{}, nil
}

func (r *Reconciler) reconcileDelete(kind *v1beta1.Kind) (reconcile.Result, error) {
	// If the kind has already been finalized, exit reconciliation
	if !k8s.HasFinalizer(kind, topoFinalizer) {
		return reconcile.Result{}, nil
	}

	// Connect to the topology service
	conn, err := grpc.ConnectService(r.client, kind.Namespace, topoService)
	if err != nil {
		return reconcile.Result{}, err
	}
	defer conn.Close()

	client := topo.NewTopoClient(conn)

	// Delete the kind from the topology
	if err := r.deleteKind(kind, client); err != nil {
		return reconcile.Result{}, err
	}

	// Once the kind has been deleted, remove the topology finalizer
	k8s.RemoveFinalizer(kind, topoFinalizer)
	if err := r.client.Update(context.TODO(), kind); err != nil {
		return reconcile.Result{}, err
	}
	return reconcile.Result{}, nil
}

func (r *Reconciler) kindExists(kind *v1beta1.Kind, client topo.TopoClient) (bool, error) {
	request := &topo.GetRequest{
		ID: topo.ID(kind.Name),
	}
	_, err := client.Get(context.TODO(), request)
	if err == nil {
		return true, nil
	}

	stat, ok := status.FromError(err)
	if !ok {
		return false, err
	}

	err = errors.FromStatus(stat)
	if !errors.IsNotFound(err) {
		return false, err
	}
	return false, nil
}

func (r *Reconciler) createKind(kind *v1beta1.Kind, client topo.TopoClient) error {
	request := &topo.CreateRequest{
		Object: &topo.Object{
			ID:   topo.ID(kind.Name),
			Type: topo.Object_KIND,
			Obj: &topo.Object_Kind{
				Kind: &topo.Kind{
					Name:       kind.Name,
					Attributes: kind.Spec.Attributes,
				},
			},
			Attributes: kind.Spec.Attributes,
		},
	}

	_, err := client.Create(context.TODO(), request)
	if err == nil {
		return nil
	}

	stat, ok := status.FromError(err)
	if !ok {
		return err
	}

	err = errors.FromStatus(stat)
	if !errors.IsAlreadyExists(err) {
		return err
	}
	return nil
}

func (r *Reconciler) deleteKind(kind *v1beta1.Kind, client topo.TopoClient) error {
	request := &topo.DeleteRequest{
		ID: topo.ID(kind.Name),
	}

	_, err := client.Delete(context.TODO(), request)
	if err == nil {
		return nil
	}

	stat, ok := status.FromError(err)
	if !ok {
		return err
	}

	err = errors.FromStatus(stat)
	if !errors.IsNotFound(err) {
		return err
	}
	return nil
}
