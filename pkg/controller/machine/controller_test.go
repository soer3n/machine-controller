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

package controller

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/go-test/deep"
	"go.uber.org/zap"

	"k8c.io/machine-controller/pkg/cloudprovider/instance"
	cloudprovidertypes "k8c.io/machine-controller/pkg/cloudprovider/types"
	clusterv1alpha1 "k8c.io/machine-controller/sdk/apis/cluster/v1alpha1"
	providerconfigtypes "k8c.io/machine-controller/sdk/providerconfig"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/tools/record"
	ctrlruntimeclient "sigs.k8s.io/controller-runtime/pkg/client"
	fakectrlruntimeclient "sigs.k8s.io/controller-runtime/pkg/client/fake"
)

func init() {
	if err := clusterv1alpha1.AddToScheme(scheme.Scheme); err != nil {
		panic(fmt.Sprintf("failed to add clusterv1alpha1 api to scheme: %v", err))
	}
}

type fakeInstance struct {
	name       string
	id         string
	providerID string
	addresses  map[string]corev1.NodeAddressType
	status     instance.Status
}

func (i *fakeInstance) Name() string {
	return i.name
}

func (i *fakeInstance) ID() string {
	return i.id
}

func (i *fakeInstance) ProviderID() string {
	return i.providerID
}

func (i *fakeInstance) Status() instance.Status {
	return i.status
}

func (i *fakeInstance) Addresses() map[string]corev1.NodeAddressType {
	return i.addresses
}

func getTestNode(id, providerID string) corev1.Node {
	return corev1.Node{
		ObjectMeta: metav1.ObjectMeta{
			Name: fmt.Sprintf("node%s", id),
		},
		Spec: corev1.NodeSpec{
			ProviderID: providerID,
		},
		Status: corev1.NodeStatus{
			Addresses: []corev1.NodeAddress{
				{
					Type:    corev1.NodeInternalIP,
					Address: fmt.Sprintf("192.168.1.%s", id),
				},
				{
					Type:    corev1.NodeExternalIP,
					Address: fmt.Sprintf("172.16.1.%s", id),
				},
			},
		},
	}
}

func TestController_GetNode(t *testing.T) {
	node1 := getTestNode("1", "aws:///i-1")
	node2 := getTestNode("2", "openstack:///test")
	node3 := getTestNode("3", "")
	node4 := getTestNode("4", "hcloud://123")
	nodeList := []*corev1.Node{&node1, &node2, &node3, &node4}

	tests := []struct {
		name     string
		instance instance.Instance
		resNode  *corev1.Node
		exists   bool
		err      error
		provider providerconfigtypes.CloudProvider
	}{
		{
			name:     "node not found - no nodeList",
			provider: "",
			resNode:  nil,
			exists:   false,
			err:      nil,
			instance: &fakeInstance{id: "99", addresses: map[string]corev1.NodeAddressType{"192.168.1.99": corev1.NodeInternalIP}},
		},
		{
			name:     "node not found - no suitable node",
			provider: "",
			resNode:  nil,
			exists:   false,
			err:      nil,
			instance: &fakeInstance{id: "99", addresses: map[string]corev1.NodeAddressType{"192.168.1.99": corev1.NodeInternalIP}},
		},
		{
			name:     "node found by provider id",
			provider: "aws",
			resNode:  &node1,
			exists:   true,
			err:      nil,
			instance: &fakeInstance{id: "1", addresses: map[string]corev1.NodeAddressType{"": ""}, providerID: "aws:///i-1"},
		},
		{
			name:     "node found by internal ip",
			provider: "",
			resNode:  &node3,
			exists:   true,
			err:      nil,
			instance: &fakeInstance{id: "3", addresses: map[string]corev1.NodeAddressType{"192.168.1.3": corev1.NodeInternalIP}},
		},
		{
			name:     "node found by external ip",
			provider: "",
			resNode:  &node3,
			exists:   true,
			err:      nil,
			instance: &fakeInstance{id: "3", addresses: map[string]corev1.NodeAddressType{"172.16.1.3": corev1.NodeInternalIP}},
		},
		{
			name:     "hetzner node found by internal ip",
			provider: "hetzner",
			resNode:  &node3,
			exists:   true,
			err:      nil,
			instance: &fakeInstance{id: "3", name: "node3", addresses: map[string]corev1.NodeAddressType{"192.168.1.3": corev1.NodeInternalIP}},
		},
		{
			name:     "hetzner node found by external ip",
			provider: "hetzner",
			resNode:  &node3,
			exists:   true,
			err:      nil,
			instance: &fakeInstance{id: "3", name: "node3", addresses: map[string]corev1.NodeAddressType{"172.16.1.3": corev1.NodeExternalIP}},
		},
		{
			name:     "hetzner node not found - node and instance names mismatch",
			provider: "hetzner",
			resNode:  nil,
			exists:   false,
			err:      nil,
			instance: &fakeInstance{id: "3", name: "instance3", addresses: map[string]corev1.NodeAddressType{"192.168.1.3": corev1.NodeInternalIP}},
		},
		{
			name:     "hetzner node found by provider id",
			provider: "hetzner",
			resNode:  &node4,
			exists:   true,
			err:      nil,
			instance: &fakeInstance{id: "4", addresses: map[string]corev1.NodeAddressType{"": ""}, providerID: "hcloud://123"},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			ctx := context.Background()

			nodes := []ctrlruntimeclient.Object{}
			for _, node := range nodeList {
				nodes = append(nodes, node)
			}
			client := fakectrlruntimeclient.NewClientBuilder().
				WithScheme(scheme.Scheme).
				WithObjects(nodes...).
				Build()

			reconciler := Reconciler{client: client}

			node, exists, err := reconciler.getNode(ctx, zap.NewNop().Sugar(), test.instance, test.provider)
			if diff := deep.Equal(err, test.err); diff != nil {
				t.Errorf("expected to get %v instead got: %v", test.err, err)
			}
			if err != nil {
				return
			}

			if exists != test.exists {
				t.Errorf("expected to get %v instead got: %v", test.exists, exists)
			}
			if !exists {
				return
			}

			if diff := deep.Equal(node, test.resNode); diff != nil {
				t.Errorf("expected to get %v instead got: %v", test.resNode, node)
			}
		})
	}
}

func TestControllerDeletesMachinesOnJoinTimeout(t *testing.T) {
	tests := []struct {
		name              string
		creationTimestamp metav1.Time
		hasNode           bool
		ownerReferences   []metav1.OwnerReference
		hasOwner          bool
		getsDeleted       bool
		joinTimeoutConfig *time.Duration
	}{
		{
			name:              "machine with node does not get deleted",
			creationTimestamp: metav1.Time{Time: time.Now().Add(-20 * time.Minute)},
			hasNode:           true,
			getsDeleted:       false,
			joinTimeoutConfig: durationPtr(10 * time.Minute),
		},
		{
			name:              "machine without owner ref does not get deleted",
			creationTimestamp: metav1.Time{Time: time.Now().Add(-20 * time.Minute)},
			hasNode:           false,
			getsDeleted:       false,
			joinTimeoutConfig: durationPtr(10 * time.Minute),
		},
		{
			name:              "machine younger than joinClusterTimeout does not get deleted",
			creationTimestamp: metav1.Time{Time: time.Now().Add(-9 * time.Minute)},
			hasNode:           false,
			ownerReferences:   []metav1.OwnerReference{{Name: "owner", Kind: "MachineSet"}},
			hasOwner:          true,
			getsDeleted:       false,
			joinTimeoutConfig: durationPtr(10 * time.Minute),
		},
		{
			name:              "machine older than joinClusterTimeout gets deleted",
			creationTimestamp: metav1.Time{Time: time.Now().Add(-20 * time.Minute)},
			hasNode:           false,
			ownerReferences:   []metav1.OwnerReference{{Name: "owner", Kind: "MachineSet"}},
			getsDeleted:       true,
			joinTimeoutConfig: durationPtr(10 * time.Minute),
		},
		{
			name:              "machine older than joinClusterTimeout does not get deleted when ownerReference.Kind != MachineSet",
			creationTimestamp: metav1.Time{Time: time.Now().Add(-20 * time.Minute)},
			hasNode:           false,
			ownerReferences:   []metav1.OwnerReference{{Name: "owner", Kind: "Cat"}},
			getsDeleted:       false,
			joinTimeoutConfig: durationPtr(10 * time.Minute),
		},
		{
			name:              "nil joinTimeoutConfig results in no deletions",
			creationTimestamp: metav1.Time{Time: time.Now().Add(-20 * time.Minute)},
			hasNode:           false,
			ownerReferences:   []metav1.OwnerReference{{Name: "owner", Kind: "MachineSet"}},
			getsDeleted:       false,
			joinTimeoutConfig: nil,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			ctx := context.Background()

			machine := &clusterv1alpha1.Machine{
				ObjectMeta: metav1.ObjectMeta{
					Name:              "my-machine",
					CreationTimestamp: test.creationTimestamp,
					OwnerReferences:   test.ownerReferences}}

			node := &corev1.Node{}
			instance := &fakeInstance{}
			if test.hasNode {
				literalNode := getTestNode("test-id", "")
				node = &literalNode
				instance.id = "test-id"
			}

			providerConfig := &providerconfigtypes.Config{CloudProvider: providerconfigtypes.CloudProviderFake}

			client := fakectrlruntimeclient.NewClientBuilder().
				WithScheme(scheme.Scheme).
				WithObjects(node, machine).
				Build()

			reconciler := Reconciler{
				client:             client,
				recorder:           &record.FakeRecorder{},
				joinClusterTimeout: test.joinTimeoutConfig,
			}

			if _, err := reconciler.ensureNodeOwnerRef(ctx, zap.NewNop().Sugar(), instance, machine, providerConfig); err != nil {
				t.Fatalf("failed to call ensureNodeOwnerRef: %v", err)
			}

			err := client.Get(ctx, types.NamespacedName{Name: machine.Name}, &clusterv1alpha1.Machine{})
			wasDeleted := apierrors.IsNotFound(err)

			if wasDeleted != test.getsDeleted {
				t.Errorf("Machine was deleted: %v, but expectedDeletion: %v", wasDeleted, test.getsDeleted)
			}
		})
	}
}

func durationPtr(d time.Duration) *time.Duration {
	return &d
}

func TestControllerShouldEvict(t *testing.T) {
	threeHoursAgo := metav1.NewTime(time.Now().Add(-3 * time.Hour))
	now := metav1.Now()
	finalizer := "test"

	tests := []struct {
		name               string
		machine            *clusterv1alpha1.Machine
		additionalMachines []ctrlruntimeclient.Object
		existingNodes      []ctrlruntimeclient.Object
		shouldEvict        bool
	}{
		{
			name:        "skip eviction due to eviction timeout",
			shouldEvict: false,
			existingNodes: []ctrlruntimeclient.Object{&corev1.Node{
				ObjectMeta: metav1.ObjectMeta{
					Name: "existing-node",
				},
			}},
			machine: &clusterv1alpha1.Machine{
				ObjectMeta: metav1.ObjectMeta{
					DeletionTimestamp: &threeHoursAgo,
					Finalizers:        []string{finalizer},
				},
				Status: clusterv1alpha1.MachineStatus{
					NodeRef: &corev1.ObjectReference{Name: "existing-node"},
				},
			},
		},
		{
			name:        "skip eviction due to no nodeRef",
			shouldEvict: false,
			machine: &clusterv1alpha1.Machine{
				ObjectMeta: metav1.ObjectMeta{
					DeletionTimestamp: &now,
					Finalizers:        []string{finalizer},
				},
				Status: clusterv1alpha1.MachineStatus{
					NodeRef: nil,
				},
			},
		},
		{
			name:        "skip eviction due to already gone node",
			shouldEvict: false,
			machine: &clusterv1alpha1.Machine{
				ObjectMeta: metav1.ObjectMeta{
					DeletionTimestamp: &now,
					Finalizers:        []string{finalizer},
				},
				Status: clusterv1alpha1.MachineStatus{
					NodeRef: &corev1.ObjectReference{Name: "non-existing-node"},
				},
			},
		},
		{
			name: "Skip eviction due to no available target",
			existingNodes: []ctrlruntimeclient.Object{&corev1.Node{
				ObjectMeta: metav1.ObjectMeta{
					Name: "existing-node",
				},
			}},
			machine: &clusterv1alpha1.Machine{
				ObjectMeta: metav1.ObjectMeta{
					DeletionTimestamp: &now,
					Finalizers:        []string{finalizer},
				},
				Status: clusterv1alpha1.MachineStatus{
					NodeRef: &corev1.ObjectReference{Name: "existing-node"},
				},
			},
		},
		{
			name:        "Eviction possible because of second node",
			shouldEvict: true,
			existingNodes: []ctrlruntimeclient.Object{&corev1.Node{
				ObjectMeta: metav1.ObjectMeta{
					Name: "existing-node",
				}}, &corev1.Node{
				ObjectMeta: metav1.ObjectMeta{
					Name: "eviction-destination",
				}},
			},
			machine: &clusterv1alpha1.Machine{
				ObjectMeta: metav1.ObjectMeta{
					DeletionTimestamp: &now,
					Finalizers:        []string{finalizer},
				},
				Status: clusterv1alpha1.MachineStatus{
					NodeRef: &corev1.ObjectReference{Name: "existing-node"},
				},
			},
		},
		{
			name:        "Eviction possible because of machine without noderef",
			shouldEvict: true,
			existingNodes: []ctrlruntimeclient.Object{&corev1.Node{
				ObjectMeta: metav1.ObjectMeta{
					Name: "existing-node",
				}}, &corev1.Node{
				ObjectMeta: metav1.ObjectMeta{
					Name: "eviction-destination",
				}},
			},
			machine: &clusterv1alpha1.Machine{
				ObjectMeta: metav1.ObjectMeta{
					DeletionTimestamp: &now,
					Finalizers:        []string{finalizer},
				},
				Status: clusterv1alpha1.MachineStatus{
					NodeRef: &corev1.ObjectReference{Name: "existing-node"},
				},
			},
			additionalMachines: []ctrlruntimeclient.Object{
				&clusterv1alpha1.Machine{
					ObjectMeta: metav1.ObjectMeta{
						Name: "new-machine-without-a-node",
					},
				},
			},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			ctx := context.Background()

			objects := []ctrlruntimeclient.Object{test.machine}
			objects = append(objects, test.existingNodes...)
			objects = append(objects, test.additionalMachines...)

			client := fakectrlruntimeclient.NewClientBuilder().
				WithScheme(scheme.Scheme).
				WithObjects(objects...).
				Build()

			reconciler := &Reconciler{
				client:            client,
				skipEvictionAfter: 2 * time.Hour,
			}

			shouldEvict, err := reconciler.shouldEvict(ctx, zap.NewNop().Sugar(), test.machine)
			if err != nil {
				t.Fatal(err)
			}

			if shouldEvict != test.shouldEvict {
				t.Errorf("Expected shouldEvict to be %v but got %v instead", test.shouldEvict, shouldEvict)
			}
		})
	}
}

func TestControllerDeleteNodeForMachine(t *testing.T) {
	machineUID := types.UID("test-1")

	tests := []struct {
		name             string
		machine          *clusterv1alpha1.Machine
		nodes            []*corev1.Node
		err              error
		shouldDeleteNode string
	}{
		{
			name: "delete node by nodeRef",
			machine: &clusterv1alpha1.Machine{
				ObjectMeta: metav1.ObjectMeta{
					Name:       "machine-1",
					Finalizers: []string{"machine-node-delete-finalizer"},
				},
				Status: clusterv1alpha1.MachineStatus{
					NodeRef: &corev1.ObjectReference{Name: "node-1"},
				},
			},
			nodes: []*corev1.Node{
				{
					ObjectMeta: metav1.ObjectMeta{
						Name: "node-0",
					},
				},
				{
					ObjectMeta: metav1.ObjectMeta{
						Name: "node-1",
					},
				},
			},
			err:              nil,
			shouldDeleteNode: "node-1",
		},
		{
			name: "delete node by NodeOwner label",
			machine: &clusterv1alpha1.Machine{
				ObjectMeta: metav1.ObjectMeta{
					Name:       "machine-1",
					Finalizers: []string{"machine-node-delete-finalizer"},
					UID:        machineUID,
				},
				Status: clusterv1alpha1.MachineStatus{},
			},
			nodes: []*corev1.Node{
				{
					ObjectMeta: metav1.ObjectMeta{
						Name: "node-0",
						Labels: map[string]string{
							NodeOwnerLabelName: string(machineUID),
						},
					},
				},
				{
					ObjectMeta: metav1.ObjectMeta{
						Name: "node-1",
					},
				},
			},
			err:              nil,
			shouldDeleteNode: "node-0",
		},
		{
			name: "no node should be deleted",
			machine: &clusterv1alpha1.Machine{
				ObjectMeta: metav1.ObjectMeta{
					Name:       "machine-1",
					Finalizers: []string{"machine-node-delete-finalizer"},
					UID:        machineUID,
				},
				Status: clusterv1alpha1.MachineStatus{},
			},
			nodes: []*corev1.Node{
				{
					ObjectMeta: metav1.ObjectMeta{
						Name: "node-0",
					},
				},
				{
					ObjectMeta: metav1.ObjectMeta{
						Name: "node-1",
					},
				},
			},
			err:              nil,
			shouldDeleteNode: "",
		},
		{
			name: "node referred by nodeRef doesn't exist",
			machine: &clusterv1alpha1.Machine{
				ObjectMeta: metav1.ObjectMeta{
					Name:       "machine-1",
					Finalizers: []string{"machine-node-delete-finalizer"},
				},
				Status: clusterv1alpha1.MachineStatus{
					NodeRef: &corev1.ObjectReference{Name: "node-1"},
				},
			},
			nodes: []*corev1.Node{
				{
					ObjectMeta: metav1.ObjectMeta{
						Name: "node-0",
					},
				},
			},
			err:              nil,
			shouldDeleteNode: "",
		},
	}

	log := zap.NewNop().Sugar()

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			ctx := context.Background()

			objects := []ctrlruntimeclient.Object{test.machine}
			for _, n := range test.nodes {
				objects = append(objects, n)
			}

			client := fakectrlruntimeclient.NewClientBuilder().
				WithScheme(scheme.Scheme).
				WithStatusSubresource().
				WithObjects(objects...).
				Build()

			providerData := &cloudprovidertypes.ProviderData{
				Ctx:    ctx,
				Update: cloudprovidertypes.GetMachineUpdater(ctx, client),
				Client: client,
			}

			reconciler := &Reconciler{
				client:       client,
				recorder:     &record.FakeRecorder{},
				providerData: providerData,
			}

			nodes, err := reconciler.retrieveNodesRelatedToMachine(ctx, log, test.machine)
			if err != nil {
				return
			}

			err = reconciler.deleteNodeForMachine(ctx, log, nodes, test.machine)
			if diff := deep.Equal(err, test.err); diff != nil {
				t.Errorf("expected to get %v instead got: %v", test.err, err)
			}
			if err != nil {
				return
			}

			if test.shouldDeleteNode != "" {
				err = client.Get(ctx, types.NamespacedName{Name: test.shouldDeleteNode}, &corev1.Node{})
				if !apierrors.IsNotFound(err) {
					t.Errorf("expected node %q to be deleted, but got: %v", test.shouldDeleteNode, err)
				}
			} else {
				nodes := &corev1.NodeList{}
				err = client.List(ctx, nodes, &ctrlruntimeclient.ListOptions{})
				if err != nil {
					t.Errorf("error listing nodes: %v", err)
				}
				if len(test.nodes) != len(nodes.Items) {
					t.Errorf("expected %d nodes, but got %d", len(test.nodes), len(nodes.Items))
				}
			}
		})
	}
}

func TestControllerFindNodeByProviderID(t *testing.T) {
	tests := []struct {
		name         string
		instance     instance.Instance
		provider     providerconfigtypes.CloudProvider
		nodes        []corev1.Node
		expectedNode bool
	}{
		{
			name:     "aws providerID type 1",
			instance: &fakeInstance{id: "99", providerID: "aws:///some-zone/i-99"},
			provider: providerconfigtypes.CloudProviderAWS,
			nodes: []corev1.Node{
				getTestNode("1", "random"),
				getTestNode("2", "aws:///some-zone/i-99"),
			},
			expectedNode: true,
		},
		{
			name:     "aws providerID type 2",
			instance: &fakeInstance{id: "99", providerID: "aws:///i-99"},
			provider: providerconfigtypes.CloudProviderAWS,
			nodes: []corev1.Node{
				getTestNode("1", "aws:///i-99"),
				getTestNode("2", "random"),
			},
			expectedNode: true,
		},
		{
			name:     "azure providerID",
			instance: &fakeInstance{id: "99", providerID: "azure:///test/test"},
			provider: providerconfigtypes.CloudProviderAWS,
			nodes: []corev1.Node{
				getTestNode("1", "random"),
				getTestNode("2", "azure:///test/test"),
			},
			expectedNode: true,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			node := findNodeByProviderID(test.instance, test.provider, test.nodes)
			if (node != nil) != test.expectedNode {
				t.Errorf("expected %t, but got %t", test.expectedNode, (node != nil))
			}
		})
	}
}
