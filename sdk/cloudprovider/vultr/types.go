/*
Copyright 2023 The Machine Controller Authors.

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

package vultr

import (
	"k8c.io/machine-controller/sdk/jsonutil"
	"k8c.io/machine-controller/sdk/providerconfig"
)

type RawConfig struct {
	PhysicalMachine bool                           `json:"physicalMachine,omitempty"`
	APIKey          providerconfig.ConfigVarString `json:"apiKey,omitempty"`
	Region          providerconfig.ConfigVarString `json:"region"`
	Plan            providerconfig.ConfigVarString `json:"plan"`
	OsID            providerconfig.ConfigVarString `json:"osId"`
	Tags            []string                       `json:"tags,omitempty"`
	VpcID           []string                       `json:"vpcId,omitempty"`
	Vpc2ID          []string                       `json:"vpc2Id,omitempty"`
	EnableVPC       bool                           `json:"enableVPC,omitempty"`
	EnableVPC2      bool                           `json:"enableVPC2,omitempty"`
	EnableIPv6      bool                           `json:"enableIPv6,omitempty"`
}

func GetConfig(pconfig providerconfig.Config) (*RawConfig, error) {
	rawConfig := &RawConfig{}

	return rawConfig, jsonutil.StrictUnmarshal(pconfig.CloudProviderSpec.Raw, rawConfig)
}
