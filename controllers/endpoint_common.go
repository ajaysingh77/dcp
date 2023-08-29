// Copyright (c) Microsoft Corporation. All rights reserved.

package controllers

import (
	"encoding/json"

	"github.com/go-logr/logr"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	ctrl_client "sigs.k8s.io/controller-runtime/pkg/client"

	apiv1 "github.com/microsoft/usvc-apiserver/api/v1"
)

const (
	serviceProducerAnnotation = "service-producer"
	workloadOwnerKey          = ".metadata.owner"
)

type ServiceWorkloadEndpointKey struct {
	NamespacedNameWithKind
	ServiceName string
}

type ContainerReconcilerOrExecutableReconciler interface {
	ctrl_client.Client
}

// func createEndpointForContainer(
// 	ctr *apiv1.Container,
// 	serviceProducer ServiceProducer,
// 	log logr.Logger,
// ) (*apiv1.Endpoint, error) {
// 	endpointName, err := MakeUniqueName(ctr.GetName())
// 	if err != nil {
// 		log.Error(err, "could not generate unique name for Endpoint object")
// 		return nil, err
// 	}

// 	if serviceProducer.Address != "" {
// 		log.Error(fmt.Errorf("address cannot be specified for Container objects"), serviceProducerIsInvalid)
// 		return nil, err
// 	}

// 	// TODO: validate port according to descirption in ServiceProducer struct below

// 	// Otherwise, create a new Endpoint object.
// 	endpoint := &apiv1.Endpoint{
// 		ObjectMeta: metav1.ObjectMeta{
// 			Name:      endpointName,
// 			Namespace: ctr.Namespace,
// 		},
// 		Spec: apiv1.EndpointSpec{
// 			ServiceNamespace: ctr.Namespace,
// 			ServiceName:      serviceProducer.ServiceName,
// 			Address:          "", // TODO: find address to use, from either container spec or inspection
// 			Port:             0,  // TODO: find port to use, from either container spec or inspection
// 		},
// 	}

// 	return endpoint, nil
// }

func createEndpointForExecutable(
	exe *apiv1.Executable,
	serviceProducer ServiceProducer,
	log logr.Logger,
) (*apiv1.Endpoint, error) {
	endpointName, err := MakeUniqueName(exe.GetName())
	if err != nil {
		log.Error(err, "could not generate unique name for Endpoint object")
		return nil, err
	}

	// TODO: logic to find an address and port for the executable
	address := serviceProducer.Address
	if address == "" {
		address = "localhost"
	}

	// Otherwise, create a new Endpoint object.
	endpoint := &apiv1.Endpoint{
		ObjectMeta: metav1.ObjectMeta{
			Name:      endpointName,
			Namespace: exe.Namespace,
		},
		Spec: apiv1.EndpointSpec{
			ServiceNamespace: exe.Namespace,
			ServiceName:      serviceProducer.ServiceName,
			Address:          address,
			Port:             serviceProducer.Port,
		},
	}

	return endpoint, nil
}

func parseServiceProducerAnnotation(annotation string) ([]ServiceProducer, error) {
	retval := make([]ServiceProducer, 0)

	// TODO: parse multiple entries when possible
	sp := ServiceProducer{}
	err := json.Unmarshal([]byte(annotation), &sp)
	retval = append(retval, sp)

	return retval, err
}

const serviceProducerIsInvalid = "service-producer annotation is invalid"

type ServiceProducer struct {
	// Name of the service that the workload implements.
	ServiceName string `json:"serviceName"`

	// Address used by the workload to serve the service.
	// In the current implementation it only applies to Executables and defaults to localhost if not present.
	// (Containers use the address specified by their Spec).
	Address string `json:"address,omitempty"`

	// Port used by the workload to serve the service. Mandatory.
	// For Containers it must match one of the Container ports.
	// We first match on HostPort, and if one is found, we use that port.
	// If no HostPort is found, we match on ContainerPort for ports that do not specify a HostPort
	// (the port is auto-allocated by Docker). If such port is found, we proxy to the auto-allocated host port.
	Port int32 `json:"port"`
}
