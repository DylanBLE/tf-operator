// Copyright 2018 The Kubeflow Authors
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//      http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

// Package controller provides a Kubernetes controller for a TFJob resource.
package tfcontroller

import (
	"fmt"
	"strconv"
	"strings"

	log "github.com/sirupsen/logrus"
	"k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"

	tfv1alpha2 "github.com/kubeflow/tf-operator/pkg/apis/tensorflow/v1alpha2"
	"github.com/kubeflow/tf-operator/pkg/controller.v2/jobcontroller"
	tflogger "github.com/kubeflow/tf-operator/pkg/logger"
)

// reconcileServices checks and updates services for each given TFReplicaSpec.
// It will requeue the tfjob in case of an error while creating/deleting services.
func (tc *TFJobController) reconcileServices(
	tfjob *tfv1alpha2.TFJob,
	services []*v1.Service,
	rtype tfv1alpha2.TFReplicaType,
	spec *tfv1alpha2.TFReplicaSpec) error {

	// Convert TFReplicaType to lower string.
	rt := strings.ToLower(string(rtype))

	replicas := int(*spec.Replicas)
	// Get all services for the type rt.
	services, err := filterServicesForTFReplicaType(services, rt)
	if err != nil {
		return err
	}

	serviceSlices := getServiceSlices(services, replicas, tflogger.LoggerForReplica(tfjob, rt))

	for index, serviceSlice := range serviceSlices {
		if len(serviceSlice) > 1 {
			tflogger.LoggerForReplica(tfjob, rt).Warningf("We have too many services for %s %d", rt, index)
			// TODO(gaocegege): Kill some services.
		} else if len(serviceSlice) == 0 {
			tflogger.LoggerForReplica(tfjob, rt).Infof("need to create new service: %s-%d", rt, index)
			err = tc.createNewService(tfjob, rtype, strconv.Itoa(index), spec)
			if err != nil {
				return err
			}
		}
	}

	return nil
}

// getServiceSlices returns a slice, which element is the slice of service.
// Assume the return object is serviceSlices, then serviceSlices[i] is an
// array of pointers to services corresponding to Services for replica i.
func getServiceSlices(services []*v1.Service, replicas int, logger *log.Entry) [][]*v1.Service {
	serviceSlices := make([][]*v1.Service, replicas)
	for _, service := range services {
		if _, ok := service.Labels[tfReplicaIndexLabel]; !ok {
			logger.Warning("The service do not have the index label.")
			continue
		}
		index, err := strconv.Atoi(service.Labels[tfReplicaIndexLabel])
		if err != nil {
			logger.Warningf("Error when strconv.Atoi: %v", err)
			continue
		}
		if index < 0 || index >= replicas {
			logger.Warningf("The label index is not expected: %d", index)
		} else {
			serviceSlices[index] = append(serviceSlices[index], service)
		}
	}
	return serviceSlices
}

// createNewService creates a new service for the given index and type.
func (tc *TFJobController) createNewService(tfjob *tfv1alpha2.TFJob, rtype tfv1alpha2.TFReplicaType, index string, spec *tfv1alpha2.TFReplicaSpec) error {
	tfjobKey, err := KeyFunc(tfjob)
	if err != nil {
		utilruntime.HandleError(fmt.Errorf("Couldn't get key for tfjob object %#v: %v", tfjob, err))
		return err
	}

	// Convert TFReplicaType to lower string.
	rt := strings.ToLower(string(rtype))
	expectationServicesKey := genExpectationServicesKey(tfjobKey, rt)
	err = tc.Expectations.ExpectCreations(expectationServicesKey, 1)
	if err != nil {
		return err
	}

	// Create OwnerReference.
	controllerRef := tc.GenOwnerReference(tfjob)

	// Append tfReplicaTypeLabel and tfReplicaIndexLabel labels.
	labels := tc.GenLabels(tfjob.Name)
	labels[tfReplicaTypeLabel] = rt
	labels[tfReplicaIndexLabel] = index

	port, err := GetPortFromTFJob(tfjob, rtype)
	if err != nil {
		return err
	}

	service := &v1.Service{
		Spec: v1.ServiceSpec{
			ClusterIP: "None",
			Selector:  labels,
			Ports: []v1.ServicePort{
				{
					Name: tfv1alpha2.DefaultPortName,
					Port: port,
				},
			},
		},
	}

	service.Name = jobcontroller.GenGeneralName(tfjob.Name, rt, index)
	service.Labels = labels

	err = tc.ServiceControl.CreateServicesWithControllerRef(tfjob.Namespace, service, tfjob, controllerRef)
	if err != nil && errors.IsTimeout(err) {
		// Service is created but its initialization has timed out.
		// If the initialization is successful eventually, the
		// controller will observe the creation via the informer.
		// If the initialization fails, or if the service keeps
		// uninitialized for a long time, the informer will not
		// receive any update, and the controller will create a new
		// service when the expectation expires.
		return nil
	} else if err != nil {
		return err
	}
	return nil
}

// filterServicesForTFReplicaType returns service belong to a TFReplicaType.
func filterServicesForTFReplicaType(services []*v1.Service, tfReplicaType string) ([]*v1.Service, error) {
	var result []*v1.Service

	tfReplicaSelector := &metav1.LabelSelector{
		MatchLabels: make(map[string]string),
	}

	tfReplicaSelector.MatchLabels[tfReplicaTypeLabel] = tfReplicaType

	for _, service := range services {
		selector, err := metav1.LabelSelectorAsSelector(tfReplicaSelector)
		if err != nil {
			return nil, err
		}
		if !selector.Matches(labels.Set(service.Labels)) {
			continue
		}
		result = append(result, service)
	}
	return result, nil
}

func genExpectationServicesKey(tfjobKey, replicaType string) string {
	return tfjobKey + "/" + strings.ToLower(replicaType) + "/services"
}

// When a service is created, enqueue the controller that manages it and update its expectations.
func (tc *TFJobController) addService(obj interface{}) {
	service := obj.(*v1.Service)
	if service.DeletionTimestamp != nil {
		// on a restart of the controller controller, it's possible a new service shows up in a state that
		// is already pending deletion. Prevent the service from being a creation observation.
		// tc.deleteService(service)
		return
	}

	// If it has a ControllerRef, that's all that matters.
	if controllerRef := metav1.GetControllerOf(service); controllerRef != nil {
		tfjob := tc.resolveControllerRef(service.Namespace, controllerRef)
		if tfjob == nil {
			return
		}

		tfjobKey, err := KeyFunc(tfjob)
		if err != nil {
			return
		}

		if _, ok := service.Labels[tfReplicaTypeLabel]; !ok {
			log.Infof("This service maybe not created by tf-operator")
			return
		}

		rtype := service.Labels[tfReplicaTypeLabel]
		expectationServicesKey := genExpectationServicesKey(tfjobKey, rtype)

		tc.Expectations.CreationObserved(expectationServicesKey)
		tc.enqueueTFJob(tfjob)

		return
	}

}

// When a service is updated, figure out what tfjob/s manage it and wake them up.
// If the labels of the service have changed we need to awaken both the old
// and new replica set. old and cur must be *v1.Service types.
func (tc *TFJobController) updateService(old, cur interface{}) {
	// TODO(CPH): handle this gracefully.
}

// When a service is deleted, enqueue the tfjob that manages the service and update its expectations.
// obj could be an *v1.Service, or a DeletionFinalStateUnknown marker item.
func (tc *TFJobController) deleteService(obj interface{}) {
	// TODO(CPH): handle this gracefully.
}
