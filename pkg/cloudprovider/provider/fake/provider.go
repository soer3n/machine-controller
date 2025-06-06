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

package fake

import (
	"context"
	"encoding/json"
	"fmt"

	"go.uber.org/zap"

	"k8c.io/machine-controller/pkg/cloudprovider/instance"
	cloudprovidertypes "k8c.io/machine-controller/pkg/cloudprovider/types"
	clusterv1alpha1 "k8c.io/machine-controller/sdk/apis/cluster/v1alpha1"
	"k8c.io/machine-controller/sdk/providerconfig"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/types"
)

type provider struct{}

type CloudProviderSpec struct {
	PassValidation bool `json:"passValidation"`
}

type CloudProviderInstance struct{}

func (f CloudProviderInstance) Name() string {
	return ""
}

func (f CloudProviderInstance) ID() string {
	return ""
}

func (f CloudProviderInstance) ProviderID() string {
	return ""
}

func (f CloudProviderInstance) Addresses() map[string]corev1.NodeAddressType {
	return nil
}

func (f CloudProviderInstance) Status() instance.Status {
	return instance.StatusUnknown
}

// New returns a fake cloud provider.
func New(_ providerconfig.ConfigVarResolver) cloudprovidertypes.Provider {
	return &provider{}
}

func (p *provider) AddDefaults(_ *zap.SugaredLogger, spec clusterv1alpha1.MachineSpec) (clusterv1alpha1.MachineSpec, error) {
	return spec, nil
}

// Validate returns success or failure based according to its FakeCloudProviderSpec.
func (p *provider) Validate(_ context.Context, log *zap.SugaredLogger, machinespec clusterv1alpha1.MachineSpec) error {
	pconfig, err := providerconfig.GetConfig(machinespec.ProviderSpec)
	if err != nil {
		return err
	}

	fakeCloudProviderSpec := CloudProviderSpec{}
	if err = json.Unmarshal(pconfig.CloudProviderSpec.Raw, &fakeCloudProviderSpec); err != nil {
		return err
	}

	if fakeCloudProviderSpec.PassValidation {
		log.Debug("Succeeding validation as requested")
		return nil
	}

	log.Debug("Failing validation as requested")
	return fmt.Errorf("failing validation as requested")
}

func (p *provider) Get(_ context.Context, _ *zap.SugaredLogger, _ *clusterv1alpha1.Machine, _ *cloudprovidertypes.ProviderData) (instance.Instance, error) {
	return CloudProviderInstance{}, nil
}

// Create creates a cloud instance according to the given machine.
func (p *provider) Create(_ context.Context, _ *zap.SugaredLogger, _ *clusterv1alpha1.Machine, _ *cloudprovidertypes.ProviderData, _ string) (instance.Instance, error) {
	return CloudProviderInstance{}, nil
}

func (p *provider) Cleanup(_ context.Context, _ *zap.SugaredLogger, _ *clusterv1alpha1.Machine, _ *cloudprovidertypes.ProviderData) (bool, error) {
	return true, nil
}

func (p *provider) MigrateUID(_ context.Context, _ *zap.SugaredLogger, _ *clusterv1alpha1.Machine, _ types.UID) error {
	return nil
}

func (p *provider) MachineMetricsLabels(_ *clusterv1alpha1.Machine) (map[string]string, error) {
	return map[string]string{}, nil
}

func (p *provider) SetMetricsForMachines(_ clusterv1alpha1.MachineList) error {
	return nil
}
