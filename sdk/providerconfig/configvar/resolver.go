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

package configvar

import (
	"context"
	"fmt"
	"os"
	"strconv"
	"time"

	"k8c.io/machine-controller/sdk/providerconfig"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrlruntimeclient "sigs.k8s.io/controller-runtime/pkg/client"
)

type Resolver struct {
	ctx    context.Context
	client ctrlruntimeclient.Client
}

func NewResolver(ctx context.Context, client ctrlruntimeclient.Client) *Resolver {
	return &Resolver{
		ctx:    ctx,
		client: client,
	}
}

var _ providerconfig.ConfigVarResolver = &Resolver{}

func (r *Resolver) GetDurationValue(configVar providerconfig.ConfigVarString) (time.Duration, error) {
	durStr, err := r.GetStringValue(configVar)
	if err != nil {
		return 0, err
	}

	return time.ParseDuration(durStr)
}

func (r *Resolver) GetDurationValueOrDefault(configVar providerconfig.ConfigVarString, defaultDuration time.Duration) (time.Duration, error) {
	durStr, err := r.GetStringValue(configVar)
	if err != nil {
		return 0, err
	}

	if durStr == "" {
		return defaultDuration, nil
	}

	duration, err := time.ParseDuration(durStr)
	if err != nil {
		return 0, err
	}

	if duration <= 0 {
		return defaultDuration, nil
	}

	return duration, nil
}

func (r *Resolver) GetStringValue(configVar providerconfig.ConfigVarString) (string, error) {
	// We need all three of these to fetch and use a secret
	if configVar.SecretKeyRef.Name != "" && configVar.SecretKeyRef.Namespace != "" && configVar.SecretKeyRef.Key != "" {
		secret := &corev1.Secret{}
		name := types.NamespacedName{Namespace: configVar.SecretKeyRef.Namespace, Name: configVar.SecretKeyRef.Name}
		if err := r.client.Get(r.ctx, name, secret); err != nil {
			return "", fmt.Errorf("error retrieving secret '%s' from namespace '%s': '%w'", configVar.SecretKeyRef.Name, configVar.SecretKeyRef.Namespace, err)
		}
		if val, ok := secret.Data[configVar.SecretKeyRef.Key]; ok {
			return string(val), nil
		}
		return "", fmt.Errorf("secret '%s' in namespace '%s' has no key '%s'", configVar.SecretKeyRef.Name, configVar.SecretKeyRef.Namespace, configVar.SecretKeyRef.Key)
	}

	// We need all three of these to fetch and use a configmap
	if configVar.ConfigMapKeyRef.Name != "" && configVar.ConfigMapKeyRef.Namespace != "" && configVar.ConfigMapKeyRef.Key != "" {
		configMap := &corev1.ConfigMap{}
		name := types.NamespacedName{Namespace: configVar.ConfigMapKeyRef.Namespace, Name: configVar.ConfigMapKeyRef.Name}
		if err := r.client.Get(r.ctx, name, configMap); err != nil {
			return "", fmt.Errorf("error retrieving configmap '%s' from namespace '%s': '%w'", configVar.ConfigMapKeyRef.Name, configVar.ConfigMapKeyRef.Namespace, err)
		}
		if val, ok := configMap.Data[configVar.ConfigMapKeyRef.Key]; ok {
			return val, nil
		}
		return "", fmt.Errorf("configmap '%s' in namespace '%s' has no key '%s'", configVar.ConfigMapKeyRef.Name, configVar.ConfigMapKeyRef.Namespace, configVar.ConfigMapKeyRef.Key)
	}

	return configVar.Value, nil
}

// GetStringValueOrEnv tries to get the value from ConfigVarString, when it fails, it falls back to
// getting the value from an environment variable specified by envVarName parameter.
func (r *Resolver) GetStringValueOrEnv(configVar providerconfig.ConfigVarString, envVarName string) (string, error) {
	cfgVar, err := r.GetStringValue(configVar)
	if err == nil && len(cfgVar) > 0 {
		return cfgVar, err
	}

	envVal, _ := os.LookupEnv(envVarName)
	return envVal, nil
}

// GetBoolValue returns a boolean from a ConfigVarBool. If there is no valid source for the boolean,
// the second bool returned will be false (to be able to differentiate between "false" and "unset").
func (r *Resolver) GetBoolValue(configVar providerconfig.ConfigVarBool) (bool, bool, error) {
	// We need all three of these to fetch and use a secret
	if configVar.SecretKeyRef.Name != "" && configVar.SecretKeyRef.Namespace != "" && configVar.SecretKeyRef.Key != "" {
		secret := &corev1.Secret{}
		name := types.NamespacedName{Namespace: configVar.SecretKeyRef.Namespace, Name: configVar.SecretKeyRef.Name}
		if err := r.client.Get(r.ctx, name, secret); err != nil {
			return false, false, fmt.Errorf("error retrieving secret '%s' from namespace '%s': '%w'", configVar.SecretKeyRef.Name, configVar.SecretKeyRef.Namespace, err)
		}
		if val, ok := secret.Data[configVar.SecretKeyRef.Key]; ok {
			boolVal, err := strconv.ParseBool(string(val))
			return boolVal, (err == nil), err
		}
		return false, false, fmt.Errorf("secret '%s' in namespace '%s' has no key '%s'", configVar.SecretKeyRef.Name, configVar.SecretKeyRef.Namespace, configVar.SecretKeyRef.Key)
	}

	// We need all three of these to fetch and use a configmap
	if configVar.ConfigMapKeyRef.Name != "" && configVar.ConfigMapKeyRef.Namespace != "" && configVar.ConfigMapKeyRef.Key != "" {
		configMap := &corev1.ConfigMap{}
		name := types.NamespacedName{Namespace: configVar.ConfigMapKeyRef.Namespace, Name: configVar.ConfigMapKeyRef.Name}
		if err := r.client.Get(r.ctx, name, configMap); err != nil {
			return false, false, fmt.Errorf("error retrieving configmap '%s' from namespace '%s': '%w'", configVar.ConfigMapKeyRef.Name, configVar.ConfigMapKeyRef.Namespace, err)
		}
		if val, ok := configMap.Data[configVar.ConfigMapKeyRef.Key]; ok {
			boolVal, err := strconv.ParseBool(val)
			return boolVal, (err == nil), err
		}
		return false, false, fmt.Errorf("configmap '%s' in namespace '%s' has no key '%s'", configVar.ConfigMapKeyRef.Name, configVar.ConfigMapKeyRef.Namespace, configVar.ConfigMapKeyRef.Key)
	}

	if configVar.Value == nil {
		return false, false, nil
	}

	return configVar.Value != nil && *configVar.Value, true, nil
}

func (r *Resolver) GetBoolValueOrEnv(configVar providerconfig.ConfigVarBool, envVarName string) (bool, error) {
	boolVal, valid, err := r.GetBoolValue(configVar)
	if valid && err == nil {
		return boolVal, nil
	}

	envVal, envValFound := os.LookupEnv(envVarName)
	if envValFound {
		envValBool, err := strconv.ParseBool(envVal)
		if err != nil {
			return false, err
		}
		return envValBool, nil
	}

	return false, nil
}
