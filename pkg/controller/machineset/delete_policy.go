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
	"math"
	"sort"

	"github.com/pkg/errors"

	clusterv1alpha1 "k8c.io/machine-controller/sdk/apis/cluster/v1alpha1"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

type (
	deletePriority     float64
	deletePriorityFunc func(machine *clusterv1alpha1.Machine) deletePriority
)

const (

	// DeleteNodeAnnotation marks nodes that will be given priority for deletion
	// when a machineset scales down. This annotation is given top priority on all delete policies.
	DeleteNodeAnnotation = "cluster.k8s.io/delete-machine"

	mustDelete    deletePriority = 100.0
	betterDelete  deletePriority = 50.0
	couldDelete   deletePriority = 20.0
	mustNotDelete deletePriority = 0.0

	secondsPerTenDays float64 = 864000
)

// maps the creation timestamp onto the 0-100 priority range.
func oldestDeletePriority(machine *clusterv1alpha1.Machine) deletePriority {
	if machine.DeletionTimestamp != nil && !machine.DeletionTimestamp.IsZero() {
		return mustDelete
	}
	if machine.Annotations != nil && machine.Annotations[DeleteNodeAnnotation] != "" {
		return mustDelete
	}
	if machine.Status.ErrorReason != nil || machine.Status.ErrorMessage != nil {
		return mustDelete
	}
	if machine.CreationTimestamp.Time.IsZero() {
		return mustNotDelete
	}
	d := metav1.Now().Sub(machine.CreationTimestamp.Time)
	if d.Seconds() < 0 {
		return mustNotDelete
	}
	return deletePriority(float64(mustDelete) * (1.0 - math.Exp(-d.Seconds()/secondsPerTenDays)))
}

func newestDeletePriority(machine *clusterv1alpha1.Machine) deletePriority {
	if machine.DeletionTimestamp != nil && !machine.DeletionTimestamp.IsZero() {
		return mustDelete
	}
	if machine.Annotations != nil && machine.Annotations[DeleteNodeAnnotation] != "" {
		return mustDelete
	}
	if machine.Status.ErrorReason != nil || machine.Status.ErrorMessage != nil {
		return mustDelete
	}
	return mustDelete - oldestDeletePriority(machine)
}

func randomDeletePolicy(machine *clusterv1alpha1.Machine) deletePriority {
	if machine.DeletionTimestamp != nil && !machine.DeletionTimestamp.IsZero() {
		return mustDelete
	}
	if machine.Annotations != nil && machine.Annotations[DeleteNodeAnnotation] != "" {
		return betterDelete
	}
	if machine.Status.ErrorReason != nil || machine.Status.ErrorMessage != nil {
		return betterDelete
	}
	return couldDelete
}

type sortableMachines struct {
	machines []*clusterv1alpha1.Machine
	priority deletePriorityFunc
}

func (m sortableMachines) Len() int { return len(m.machines) }

func (m sortableMachines) Swap(i, j int) { m.machines[i], m.machines[j] = m.machines[j], m.machines[i] }

func (m sortableMachines) Less(i, j int) bool {
	return m.priority(m.machines[j]) < m.priority(m.machines[i]) // high to low
}

func getMachinesToDeletePrioritized(filteredMachines []*clusterv1alpha1.Machine, diff int, fun deletePriorityFunc) []*clusterv1alpha1.Machine {
	if diff >= len(filteredMachines) {
		return filteredMachines
	} else if diff <= 0 {
		return []*clusterv1alpha1.Machine{}
	}

	sortable := sortableMachines{
		machines: filteredMachines,
		priority: fun,
	}
	sort.Sort(sortable)

	return sortable.machines[:diff]
}

func getDeletePriorityFunc(ms *clusterv1alpha1.MachineSet) (deletePriorityFunc, error) {
	// Map the Spec.DeletePolicy value to the appropriate delete priority function
	switch msdp := clusterv1alpha1.MachineSetDeletePolicy(ms.Spec.DeletePolicy); msdp {
	case clusterv1alpha1.RandomMachineSetDeletePolicy:
		return randomDeletePolicy, nil
	case clusterv1alpha1.NewestMachineSetDeletePolicy:
		return newestDeletePriority, nil
	case clusterv1alpha1.OldestMachineSetDeletePolicy:
		return oldestDeletePriority, nil
	case "":
		return randomDeletePolicy, nil
	default:
		return nil, errors.Errorf("Unsupported delete policy %q. Must be one of 'Random', 'Newest', or 'Oldest'", msdp)
	}
}
