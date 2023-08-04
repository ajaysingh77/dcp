// Copyright (c) Microsoft Corporation. All rights reserved.

package controllers

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/go-logr/logr"
	apimachinery_errors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	ctrl_client "sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/predicate"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	apiv1 "github.com/microsoft/usvc-apiserver/api/v1"
)

type ServiceReconciler struct {
	ctrl_client.Client
	Log logr.Logger
}

var (
	serviceFinalizer string = fmt.Sprintf("%s/service-reconciler", apiv1.GroupVersion.Group)
)

func NewServiceReconciler(client ctrl_client.Client, log logr.Logger) *ServiceReconciler {
	r := ServiceReconciler{
		Client: client,
		Log:    log,
	}
	return &r
}

func (r *ServiceReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&apiv1.Service{}).
		Watches(&apiv1.Endpoint{}, handler.EnqueueRequestsFromMapFunc(requestReconcileForEndpoint), builder.WithPredicates(predicate.ResourceVersionChangedPredicate{})).
		Complete(r)
}

func requestReconcileForEndpoint(ctx context.Context, obj ctrl_client.Object) []reconcile.Request {
	endpoint := obj.(*apiv1.Endpoint)
	return []reconcile.Request{
		{
			NamespacedName: types.NamespacedName{
				Namespace: endpoint.Namespace,
				Name:      endpoint.Spec.Service,
			},
		},
	}
}

func (r *ServiceReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	log := r.Log.WithValues("ServiceName", req.NamespacedName)

	log.Info(req.Namespace, req.Name)

	select {
	case _, isOpen := <-ctx.Done():
		if !isOpen {
			log.Info("Request context expired, nothing to do...")
			return ctrl.Result{}, nil
		}
	default: // not done, proceed
	}

	svc := apiv1.Service{}
	err := r.Get(ctx, req.NamespacedName, &svc)

	if err != nil {
		if apimachinery_errors.IsNotFound(err) {
			log.Info("the Service object was deleted")
			return ctrl.Result{}, nil
		} else {
			log.Error(err, "failed to Get() the Service object")
			return ctrl.Result{}, err
		}
	}

	var change objectChange
	patch := ctrl_client.MergeFrom(svc.DeepCopy())

	if svc.DeletionTimestamp != nil && !svc.DeletionTimestamp.IsZero() {
		log.Info("Service object is being deleted")
		err := r.deleteService(ctx, svc.Spec.Name, log)
		if err != nil {
			// deleteService() logged the error already
			change = additionalReconciliationNeeded
		} else {
			change = deleteFinalizer(&svc, serviceFinalizer)
		}
	} else {
		change = ensureFinalizer(&svc, serviceFinalizer)
		change |= r.ensureService(ctx, svc.Spec.Name, log)
	}

	if (change & (metadataChanged | specChanged)) != 0 {
		err = r.Patch(ctx, &svc, patch)
		if err != nil {
			log.Error(err, "Service object update failed")
			return ctrl.Result{}, err
		} else {
			log.Info("Service object update succeeded")
		}
	}

	if (change & additionalReconciliationNeeded) != 0 {
		return ctrl.Result{RequeueAfter: additionalReconciliationDelay}, nil
	} else {
		return ctrl.Result{}, nil
	}
}

func (r *ServiceReconciler) deleteService(ctx context.Context, serviceName string, log logr.Logger) error {
	// const force = false

	// removed, err := r.orchestrator.RemoveVolumes(ctx, []string{volumeName}, force)
	// if err != nil {
	// 	if err != ct.ErrNotFound {
	// 		log.Error(err, "could not remove a container volume")
	// 		return err
	// 	} else {
	// 		return nil // If the volume is not there, that's the desired state.
	// 	}
	// } else if len(removed) != 1 || removed[0] != volumeName {
	// 	log.Error(fmt.Errorf("Unexpected response received from container volume removal request. Number of volumes removed: %d", len(removed)), "")
	// 	// .. but it did not fail, so assume the volume was removed.
	// }

	return nil
}

func (r *ServiceReconciler) ensureService(ctx context.Context, serviceName string, log logr.Logger) objectChange {
	serviceName = strings.TrimSpace(serviceName)
	if serviceName == "" {
		log.Error(fmt.Errorf("specified service name is empty"), "")

		// Hopefully someone will notice the error and update the Spec.
		// Once the Spec is changed, another reconciliation will kick in automatically.
		return noChange
	}

	if err := r.startProxyIfNeeded(ctx); err != nil {
		log.Error(err, "could not start the proxy")
		return additionalReconciliationNeeded
	}

	_, err := getServiceConfigFile(serviceName)
	if err != nil {
		log.Error(err, "could not get service config file")
		return additionalReconciliationNeeded
	}

	// Open config file and defer close
	// Write config file

	// _, err := r.orchestrator.InspectVolumes(ctx, []string{volumeName})
	// if err == nil {
	// 	return noChange // Volume exists, nothing to do
	// } else if !errors.Is(err, ct.ErrNotFound) {
	// 	log.Error(err, "could not determine whether volume exists")
	// 	return additionalReconciliationNeeded
	// }

	// err = r.orchestrator.CreateVolume(ctx, volumeName)
	// if err != nil {
	// 	log.Error(err, "could not create a volume")
	// 	return additionalReconciliationNeeded
	// }

	log.Info("service created")
	return noChange
}

func (r *ServiceReconciler) startProxyIfNeeded(ctx context.Context) error {
	return nil
}

func (r *ServiceReconciler) stopProxyIfNeeded(ctx context.Context) error {
	return nil
}

func getServiceConfigTempDir() (string, error) {
	const pattern = "usvc-apiserver-servicecontroller-serviceconfig"

	dir := filepath.Join(os.TempDir(), pattern)

	if _, err := os.Stat(dir); errors.Is(err, os.ErrNotExist) {
		return os.MkdirTemp("", pattern)
	} else if err != nil {
		return "", err
	}

	return dir, nil
}

func getServiceConfigFile(name string) (*os.File, error) {
	if dir, err := getServiceConfigTempDir(); err != nil {
		return nil, err
	} else {
		return os.CreateTemp(dir, fmt.Sprintf("%s.yaml", name))
	}
}

func deleteServiceConfigFile(name string) error {
	dir, err := getServiceConfigTempDir()
	if err != nil {
		return err
	}

	if err := os.Remove(filepath.Join(dir, fmt.Sprintf("%s.yaml", name))); err != nil {
		return err
	}

	if isConfigDirEmpty, err := isEmpty(dir); err != nil {
		return err
	} else if isConfigDirEmpty {
		return os.Remove(dir)
	}

	return nil
}

func isEmpty(dir string) (bool, error) {
	f, err := os.Open(dir)
	if err != nil {
		return false, err
	}
	defer f.Close()

	_, err = f.Readdirnames(1)
	if errors.Is(err, io.EOF) {
		return true, nil
	} else {
		return false, err
	}
}
