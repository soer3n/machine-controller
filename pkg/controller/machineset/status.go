/*
Copyright 2019 The Machine Controller Authors.

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

package machineset

import (
	"context"
	"time"

	"github.com/pkg/errors"
	"go.uber.org/zap"

	clusterv1alpha1 "k8c.io/machine-controller/sdk/apis/cluster/v1alpha1"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	ctrlruntimeclient "sigs.k8s.io/controller-runtime/pkg/client"
)

const (
	// The number of times we retry updating a MachineSet's status.
	statusUpdateRetries = 1
)

func (c *ReconcileMachineSet) calculateStatus(ctx context.Context, log *zap.SugaredLogger, ms *clusterv1alpha1.MachineSet, filteredMachines []*clusterv1alpha1.Machine) clusterv1alpha1.MachineSetStatus {
	newStatus := ms.Status
	// Count the number of machines that have labels matching the labels of the machine
	// template of the replica set, the matching machines may have more
	// labels than are in the template. Because the label of machineTemplateSpec is
	// a superset of the selector of the replica set, so the possible
	// matching machines must be part of the filteredMachines.
	fullyLabeledReplicasCount := 0
	readyReplicasCount := 0
	availableReplicasCount := 0
	templateLabel := labels.Set(ms.Spec.Template.Labels).AsSelectorPreValidated()
	for _, machine := range filteredMachines {
		if templateLabel.Matches(labels.Set(machine.Labels)) {
			fullyLabeledReplicasCount++
		}
		node, err := c.getMachineNode(ctx, machine)
		if err != nil {
			log.Debugw("Failed to get node for machine", "machine", ctrlruntimeclient.ObjectKeyFromObject(machine), zap.Error(err))
			continue
		}
		if isNodeReady(node) {
			readyReplicasCount++
			if isNodeAvailable(node, ms.Spec.MinReadySeconds, metav1.Now()) {
				availableReplicasCount++
			}
		}
	}

	newStatus.Replicas = int32(len(filteredMachines))
	newStatus.FullyLabeledReplicas = int32(fullyLabeledReplicasCount)
	newStatus.ReadyReplicas = int32(readyReplicasCount)
	newStatus.AvailableReplicas = int32(availableReplicasCount)
	return newStatus
}

// updateMachineSetStatus attempts to update the Status.Replicas of the given MachineSet, with a single GET/PUT retry.
func updateMachineSetStatus(ctx context.Context, log *zap.SugaredLogger, c ctrlruntimeclient.Client, ms *clusterv1alpha1.MachineSet, newStatus clusterv1alpha1.MachineSetStatus) (*clusterv1alpha1.MachineSet, error) {
	// This is the steady state. It happens when the MachineSet doesn't have any expectations, since
	// we do a periodic relist every 30s. If the generations differ but the replicas are
	// the same, a caller might've resized to the same replica count.
	if ms.Status.Replicas == newStatus.Replicas &&
		ms.Status.FullyLabeledReplicas == newStatus.FullyLabeledReplicas &&
		ms.Status.ReadyReplicas == newStatus.ReadyReplicas &&
		ms.Status.AvailableReplicas == newStatus.AvailableReplicas &&
		ms.Generation == ms.Status.ObservedGeneration {
		return ms, nil
	}

	// Save the generation number we acted on, otherwise we might wrongfully indicate
	// that we've seen a spec update when we retry.
	// TODO: This can clobber an update if we allow multiple agents to write to the
	// same status.
	newStatus.ObservedGeneration = ms.Generation

	var getErr, updateErr error
	for i := 0; ; i++ {
		var replicas int32
		if ms.Spec.Replicas != nil {
			replicas = *ms.Spec.Replicas
		}

		log.Debugw("Updating status",
			"specreplicas", replicas,
			"oldreplicas", ms.Status.Replicas,
			"newreplicas", newStatus.Replicas,
			"oldlabeledreplicas", ms.Status.FullyLabeledReplicas,
			"newlabeledreplicas", newStatus.FullyLabeledReplicas,
			"oldreadyreplicas", ms.Status.ReadyReplicas,
			"newreadyreplicas", newStatus.ReadyReplicas,
			"oldavailablereplicas", ms.Status.AvailableReplicas,
			"newavailablereplicas", newStatus.AvailableReplicas,
			"oldobservedgeneration", ms.Status.ObservedGeneration,
			"newobservedgeneration", newStatus.ObservedGeneration,
		)

		ms.Status = newStatus
		updateErr = c.Status().Update(ctx, ms)
		if updateErr == nil {
			return ms, nil
		}
		// Stop retrying if we exceed statusUpdateRetries - the machineSet will be requeued with a rate limit.
		if i >= statusUpdateRetries {
			break
		}
		// Update the MachineSet with the latest resource version for the next poll
		if getErr = c.Get(ctx, ctrlruntimeclient.ObjectKey{Namespace: ms.Namespace, Name: ms.Name}, ms); getErr != nil {
			// If the GET fails we can't trust status.Replicas anymore. This error
			// is bound to be more interesting than the update failure.
			return nil, getErr
		}
	}

	return nil, updateErr
}

func (c *ReconcileMachineSet) getMachineNode(ctx context.Context, machine *clusterv1alpha1.Machine) (*corev1.Node, error) {
	nodeRef := machine.Status.NodeRef
	if nodeRef == nil {
		return nil, errors.New("machine has no node ref")
	}

	node := &corev1.Node{}
	err := c.Get(ctx, ctrlruntimeclient.ObjectKey{Name: nodeRef.Name}, node)
	return node, err
}

// isNodeReady returns true if a node is ready; false otherwise.
func isNodeReady(node *corev1.Node) bool {
	if node == nil {
		return false
	}
	for _, c := range node.Status.Conditions {
		if c.Type == corev1.NodeReady {
			return c.Status == corev1.ConditionTrue
		}
	}
	return false
}

// getReadyCondition extracts the ready condition from the given status and returns that.
// Returns nil if the condition is not present.
func getReadyCondition(status *corev1.NodeStatus) *corev1.NodeCondition {
	if status == nil {
		return nil
	}
	for i := range status.Conditions {
		if status.Conditions[i].Type == corev1.NodeReady {
			return &status.Conditions[i]
		}
	}
	return nil
}

// isNodeAvailable returns true if the node is ready and minReadySeconds have elapsed or is 0. False otherwise.
func isNodeAvailable(node *corev1.Node, minReadySeconds int32, now metav1.Time) bool {
	if !isNodeReady(node) {
		return false
	}

	if minReadySeconds == 0 {
		return true
	}

	minReadySecondsDuration := time.Duration(minReadySeconds) * time.Second
	readyCondition := getReadyCondition(&node.Status)

	if !readyCondition.LastTransitionTime.IsZero() &&
		readyCondition.LastTransitionTime.Add(minReadySecondsDuration).Before(now.Time) {
		return true
	}

	return false
}
