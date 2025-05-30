/*
Copyright 2020 The Machine Controller Authors.

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

package anexia

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"go.anx.io/go-anxcloud/pkg/api"
	anxclient "go.anx.io/go-anxcloud/pkg/client"
	"go.anx.io/go-anxcloud/pkg/vsphere"
	"go.anx.io/go-anxcloud/pkg/vsphere/provisioning/progress"
	anxvm "go.anx.io/go-anxcloud/pkg/vsphere/provisioning/vm"
	"go.uber.org/zap"

	"k8c.io/machine-controller/pkg/cloudprovider/common/ssh"
	cloudprovidererrors "k8c.io/machine-controller/pkg/cloudprovider/errors"
	"k8c.io/machine-controller/pkg/cloudprovider/instance"
	cloudprovidertypes "k8c.io/machine-controller/pkg/cloudprovider/types"
	cloudproviderutil "k8c.io/machine-controller/pkg/cloudprovider/util"
	"k8c.io/machine-controller/sdk/apis/cluster/common"
	clusterv1alpha1 "k8c.io/machine-controller/sdk/apis/cluster/v1alpha1"
	anxtypes "k8c.io/machine-controller/sdk/cloudprovider/anexia"
	"k8c.io/machine-controller/sdk/providerconfig"

	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	k8stypes "k8s.io/apimachinery/pkg/types"
	kerrors "k8s.io/apimachinery/pkg/util/errors"
)

const (
	ProvisionedType = "Provisioned"
)

type provider struct {
	configVarResolver providerconfig.ConfigVarResolver
}

func (p *provider) Create(ctx context.Context, log *zap.SugaredLogger, machine *clusterv1alpha1.Machine, data *cloudprovidertypes.ProviderData, userdata string) (instance instance.Instance, retErr error) {
	status := getProviderStatus(log, machine)
	log.Debugw("Machine status", "status", status)

	// ensure conditions are present on machine
	ensureConditions(&status)

	config, providerCfg, err := p.getConfig(ctx, log, machine.Spec.ProviderSpec)
	if err != nil {
		return nil, fmt.Errorf("failed to get provider config: %w", err)
	}

	ctx = createReconcileContext(ctx, reconcileContext{
		Status:         &status,
		UserData:       userdata,
		Config:         *config,
		ProviderData:   data,
		ProviderConfig: providerCfg,
		Machine:        machine,
	})

	_, client, err := getClient(config.Token, &machine.Name)
	if err != nil {
		return nil, err
	}

	// make sure status is reflected in Machine Object
	defer func() {
		// if error occurs during updating the machine object don't override the original error
		retErr = kerrors.NewAggregate([]error{retErr, updateMachineStatus(machine, status, data.Update)})
	}()

	// provision machine
	err = provisionVM(ctx, log, client)
	if err != nil {
		return nil, anexiaErrorToTerminalError(err, "failed waiting for vm provisioning")
	}
	return p.Get(ctx, log, machine, data)
}

func provisionVM(ctx context.Context, log *zap.SugaredLogger, client anxclient.Client) error {
	reconcileContext := getReconcileContext(ctx)
	vmAPI := vsphere.NewAPI(client)

	ctx, cancel := context.WithTimeout(ctx, anxtypes.CreateRequestTimeout)
	defer cancel()

	status := reconcileContext.Status
	if status.ProvisioningID == "" {
		log.Info("Machine does not contain a provisioningID yet. Starting to provision")

		config := reconcileContext.Config
		networkInterfaces, err := networkInterfacesForProvisioning(ctx, log, client)
		if err != nil {
			return fmt.Errorf("error generating network config for machine: %w", err)
		}

		vm := vmAPI.Provisioning().VM().NewDefinition(
			config.LocationID,
			"templates",
			config.TemplateID,
			reconcileContext.Machine.Name,
			config.CPUs,
			config.Memory,
			config.Disks[0].Size,
			networkInterfaces,
		)

		vm.DiskType = config.Disks[0].PerformanceType

		if config.CPUPerformanceType != "" {
			vm.CPUPerformanceType = config.CPUPerformanceType
		}

		for _, disk := range config.Disks[1:] {
			vm.AdditionalDisks = append(vm.AdditionalDisks, anxvm.AdditionalDisk{
				SizeGBs: disk.Size,
				Type:    disk.PerformanceType,
			})
		}

		vm.Script = base64.StdEncoding.EncodeToString([]byte(reconcileContext.UserData))

		providerCfg := reconcileContext.ProviderConfig
		if providerCfg.Network != nil {
			for index, dnsServer := range providerCfg.Network.DNS.Servers {
				switch index {
				case 0:
					vm.DNS1 = dnsServer
				case 1:
					vm.DNS2 = dnsServer
				case 2:
					vm.DNS3 = dnsServer
				case 3:
					vm.DNS4 = dnsServer
				}
			}
		}

		// We generate a fresh SSH key but will never actually use it - we just want a valid public key to disable password authentication for our fresh VM.
		sshKey, err := ssh.NewKey()
		if err != nil {
			return newError(common.CreateMachineError, "failed to generate ssh key: %v", err)
		}
		vm.SSH = sshKey.PublicKey

		provisionResponse, err := vmAPI.Provisioning().VM().Provision(ctx, vm, false)
		meta.SetStatusCondition(&status.Conditions, metav1.Condition{
			Type:    ProvisionedType,
			Status:  metav1.ConditionFalse,
			Reason:  "Provisioning",
			Message: "provisioning request was sent",
		})
		if err != nil {
			return newError(common.CreateMachineError, "instance provisioning failed: %v", err)
		}

		// we successfully sent a VM provisioning request to the API, we consider the IP as 'Bound' now
		networkStatusMarkIPsBound(status)

		status.ProvisioningID = provisionResponse.Identifier
		err = updateMachineStatus(reconcileContext.Machine, *status, reconcileContext.ProviderData.Update)
		if err != nil {
			return err
		}
	}

	log.Info("Using provisionID from machine to await completion")

	meta.SetStatusCondition(&status.Conditions, metav1.Condition{
		Type:    ProvisionedType,
		Status:  metav1.ConditionTrue,
		Reason:  "Provisioned",
		Message: "Machine has been successfully created",
	})

	return updateMachineStatus(reconcileContext.Machine, *status, reconcileContext.ProviderData.Update)
}

func isAlreadyProvisioning(ctx context.Context) bool {
	status := getReconcileContext(ctx).Status
	condition := meta.FindStatusCondition(status.Conditions, ProvisionedType)
	lastChange := condition.LastTransitionTime.Time
	const reasonInProvisioning = "InProvisioning"
	if condition.Reason == reasonInProvisioning && time.Since(lastChange) > 5*time.Minute {
		meta.SetStatusCondition(&status.Conditions, metav1.Condition{
			Type:    ProvisionedType,
			Reason:  "ReInitialising",
			Message: "Could not find ongoing VM provisioning",
			Status:  metav1.ConditionFalse,
		})
	}

	return condition.Status == metav1.ConditionFalse && condition.Reason == reasonInProvisioning
}

func ensureConditions(status *anxtypes.ProviderStatus) {
	conditions := [...]metav1.Condition{
		{Type: ProvisionedType, Message: "", Status: metav1.ConditionUnknown, Reason: "Initialising"},
	}
	for _, condition := range conditions {
		if meta.FindStatusCondition(status.Conditions, condition.Type) == nil {
			meta.SetStatusCondition(&status.Conditions, condition)
		}
	}
}

func (p *provider) getConfig(ctx context.Context, log *zap.SugaredLogger, provSpec clusterv1alpha1.ProviderSpec) (*resolvedConfig, *providerconfig.Config, error) {
	pconfig, err := providerconfig.GetConfig(provSpec)
	if err != nil {
		return nil, nil, err
	}

	if pconfig.OperatingSystemSpec.Raw == nil {
		return nil, nil, errors.New("operatingSystemSpec in the MachineDeployment cannot be empty")
	}

	rawConfig, err := anxtypes.GetConfig(*pconfig)
	if err != nil {
		return nil, nil, fmt.Errorf("error parsing provider config: %w", err)
	}

	resolvedConfig, err := p.resolveConfig(ctx, log, *rawConfig)
	if err != nil {
		return nil, nil, fmt.Errorf("error resolving config: %w", err)
	}

	return resolvedConfig, pconfig, nil
}

// New returns an Anexia provider.
func New(configVarResolver providerconfig.ConfigVarResolver) cloudprovidertypes.Provider {
	return &provider{configVarResolver: configVarResolver}
}

// AddDefaults adds omitted optional values to the given MachineSpec.
func (p *provider) AddDefaults(_ *zap.SugaredLogger, spec clusterv1alpha1.MachineSpec) (clusterv1alpha1.MachineSpec, error) {
	return spec, nil
}

// Validate returns success or failure based according to its ProviderSpec.
func (p *provider) Validate(ctx context.Context, log *zap.SugaredLogger, machinespec clusterv1alpha1.MachineSpec) error {
	config, _, err := p.getConfig(ctx, log, machinespec.ProviderSpec)
	if err != nil {
		return fmt.Errorf("failed to parse config: %w", err)
	}

	if config.Token == "" {
		return errors.New("token not set")
	}

	if config.CPUs == 0 {
		return errors.New("cpu count is missing")
	}

	if len(config.Disks) == 0 {
		return errors.New("no disks configured")
	}

	for _, disk := range config.Disks {
		if disk.Size == 0 {
			return errors.New("disk size is missing")
		}
	}

	if config.Memory == 0 {
		return errors.New("memory size is missing")
	}

	if config.LocationID == "" {
		return errors.New("location id is missing")
	}

	if config.TemplateID == "" {
		return errors.New("no valid template configured")
	}

	if len(config.Networks) == 0 {
		return errors.New("no networks configured")
	}

	atLeastOneAddressSourceConfigured := false
	for _, network := range config.Networks {
		if len(network.Prefixes) > 0 {
			atLeastOneAddressSourceConfigured = true
			break
		}
	}
	if !atLeastOneAddressSourceConfigured {
		return errors.New("none of the configured networks define an address source, cannot create Machines without any IP")
	}

	return nil
}

func (p *provider) Get(ctx context.Context, log *zap.SugaredLogger, machine *clusterv1alpha1.Machine, pd *cloudprovidertypes.ProviderData) (instance.Instance, error) {
	config, _, err := p.getConfig(ctx, log, machine.Spec.ProviderSpec)
	if err != nil {
		return nil, newError(common.InvalidConfigurationMachineError, "failed to retrieve config: %v", err)
	}

	_, cli, err := getClient(config.Token, &machine.Name)
	if err != nil {
		return nil, newError(common.InvalidConfigurationMachineError, "failed to create Anexia client: %v", err)
	}
	vsphereAPI := vsphere.NewAPI(cli)

	status := getProviderStatus(log, machine)

	if status.InstanceID == "" && status.ProvisioningID == "" {
		return nil, cloudprovidererrors.ErrInstanceNotFound
	}

	if status.DeprovisioningID != "" {
		// info endpoint no longer available for vm -> stop here
		return &anexiaInstance{isDeleting: true}, nil
	}

	if status.InstanceID == "" {
		progress, err := vsphereAPI.Provisioning().Progress().Get(ctx, status.ProvisioningID)
		if err != nil {
			return nil, anexiaErrorToTerminalError(err, "failed to get provisioning progress")
		}
		if len(progress.Errors) > 0 {
			return nil, fmt.Errorf("vm provisioning had errors: %s", strings.Join(progress.Errors, ","))
		}
		if progress.Progress < 100 || progress.VMIdentifier == "" {
			return &anexiaInstance{isCreating: true}, nil
		}

		status.InstanceID = progress.VMIdentifier

		if err := updateMachineStatus(machine, status, pd.Update); err != nil {
			return nil, fmt.Errorf("failed updating machine status: %w", err)
		}
	}

	instance := anexiaInstance{}
	instance.reservedAddresses = networkReservedAddresses(&status)

	timeoutCtx, cancel := context.WithTimeout(ctx, anxtypes.GetRequestTimeout)
	defer cancel()

	info, err := vsphereAPI.Info().Get(timeoutCtx, status.InstanceID)
	if err != nil {
		return nil, anexiaErrorToTerminalError(err, "failed getting machine info")
	}
	instance.info = &info

	return &instance, nil
}

func (p *provider) Cleanup(ctx context.Context, log *zap.SugaredLogger, machine *clusterv1alpha1.Machine, data *cloudprovidertypes.ProviderData) (isDeleted bool, retErr error) {
	if inst, err := p.Get(ctx, log, machine, data); err != nil {
		if cloudprovidererrors.IsNotFound(err) {
			return true, nil
		}

		return false, err
	} else if inst.Status() == instance.StatusCreating {
		log.Error("Failed to cleanup machine: instance is still creating")
		return false, nil
	}

	status := getProviderStatus(log, machine)
	// make sure status is reflected in Machine Object
	defer func() {
		// if error occurs during updating the machine object don't override the original error
		retErr = kerrors.NewAggregate([]error{retErr, updateMachineStatus(machine, status, data.Update)})
	}()

	ensureConditions(&status)
	config, _, err := p.getConfig(ctx, log, machine.Spec.ProviderSpec)
	if err != nil {
		return false, newError(common.InvalidConfigurationMachineError, "failed to parse MachineSpec: %v", err)
	}

	_, cli, err := getClient(config.Token, &machine.Name)
	if err != nil {
		return false, newError(common.InvalidConfigurationMachineError, "failed to create Anexia client: %v", err)
	}

	vsphereAPI := vsphere.NewAPI(cli)

	deleteCtx, cancel := context.WithTimeout(ctx, anxtypes.DeleteRequestTimeout)
	defer cancel()

	// first check whether there is an provisioning ongoing
	if status.DeprovisioningID == "" {
		response, err := vsphereAPI.Provisioning().VM().Deprovision(deleteCtx, status.InstanceID, false)
		if err != nil {
			var respErr *anxclient.ResponseError

			// Only error if the error was not "not found"
			if !errors.As(err, &respErr) || respErr.ErrorData.Code != http.StatusNotFound {
				return false, newError(common.DeleteMachineError, "failed to delete machine: %v", err)
			}

			// good thinking checking for a "not found" error, but go-anxcloud does only
			// return >= 500 && < 600 errors (:
			// since that's the legacy client in go-anxcloud and the new one is not yet available,
			// this will not be fixed there but we have a nice workaround here:

			if response.Identifier == "" {
				return true, nil
			}
		}
		status.DeprovisioningID = response.Identifier
	}

	return isTaskDone(deleteCtx, cli, status.DeprovisioningID)
}

func isTaskDone(ctx context.Context, cli anxclient.Client, progressIdentifier string) (bool, error) {
	response, err := progress.NewAPI(cli).Get(ctx, progressIdentifier)
	if err != nil {
		return false, err
	}

	if len(response.Errors) != 0 {
		taskErrors, _ := json.Marshal(response.Errors)
		return true, fmt.Errorf("task failed with: %s", taskErrors)
	}

	if response.Progress == 100 {
		return true, nil
	}

	return false, nil
}

func (p *provider) MigrateUID(_ context.Context, _ *zap.SugaredLogger, _ *clusterv1alpha1.Machine, _ k8stypes.UID) error {
	return nil
}

func (p *provider) MachineMetricsLabels(_ *clusterv1alpha1.Machine) (map[string]string, error) {
	return map[string]string{}, nil
}

func (p *provider) SetMetricsForMachines(_ clusterv1alpha1.MachineList) error {
	return nil
}

func getClient(token string, machineName *string) (api.API, anxclient.Client, error) {
	logPrefix := "[Anexia API]"

	if machineName != nil {
		logPrefix = fmt.Sprintf("[Anexia API for Machine %q]", *machineName)
	}

	httpClient := cloudproviderutil.HTTPClientConfig{
		Timeout:   120 * time.Second,
		LogPrefix: logPrefix,
	}.New()

	legacyClientOptions := []anxclient.Option{
		anxclient.TokenFromString(token),
		anxclient.HTTPClient(&httpClient),
	}

	a, err := api.NewAPI(api.WithClientOptions(legacyClientOptions...))
	if err != nil {
		return nil, nil, fmt.Errorf("error creating generic API client: %w", err)
	}

	legacyClient, err := anxclient.New(legacyClientOptions...)
	if err != nil {
		return nil, nil, fmt.Errorf("error creating legacy client: %w", err)
	}

	return a, legacyClient, nil
}

func getProviderStatus(log *zap.SugaredLogger, machine *clusterv1alpha1.Machine) anxtypes.ProviderStatus {
	var providerStatus anxtypes.ProviderStatus
	status := machine.Status.ProviderStatus
	if status != nil && status.Raw != nil {
		if err := json.Unmarshal(status.Raw, &providerStatus); err != nil {
			log.Error("Failed to parse status from machine object; status was discarded for machine")
			return anxtypes.ProviderStatus{}
		}
	}
	return providerStatus
}

// newError creates a terminal error matching to the provider interface.
func newError(reason common.MachineStatusError, msg string, args ...interface{}) error {
	return cloudprovidererrors.TerminalError{
		Reason:  reason,
		Message: fmt.Sprintf(msg, args...),
	}
}

// updateMachineStatus tries to update the machine status by any means
// an error will lead to a panic.
func updateMachineStatus(machine *clusterv1alpha1.Machine, status anxtypes.ProviderStatus, updater cloudprovidertypes.MachineUpdater) error {
	rawStatus, err := json.Marshal(status)
	if err != nil {
		return err
	}
	err = updater(machine, func(machine *clusterv1alpha1.Machine) {
		machine.Status.ProviderStatus = &runtime.RawExtension{
			Raw: rawStatus,
		}
	})

	if err != nil {
		return err
	}

	return nil
}

func anexiaErrorToTerminalError(err error, msg string) error {
	var httpError api.HTTPError
	if errors.As(err, &httpError) && (httpError.StatusCode() == http.StatusForbidden || httpError.StatusCode() == http.StatusUnauthorized) {
		return cloudprovidererrors.TerminalError{
			Reason:  common.InvalidConfigurationMachineError,
			Message: "Request was rejected due to invalid credentials",
		}
	}

	var responseError *anxclient.ResponseError
	if errors.As(err, &responseError) && (responseError.ErrorData.Code == http.StatusForbidden || responseError.ErrorData.Code == http.StatusUnauthorized) {
		return cloudprovidererrors.TerminalError{
			Reason:  common.InvalidConfigurationMachineError,
			Message: "Request was rejected due to invalid credentials",
		}
	}

	return fmt.Errorf("%s: %w", msg, err)
}
