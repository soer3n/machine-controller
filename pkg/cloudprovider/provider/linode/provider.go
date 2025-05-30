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

package linode

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/linode/linodego"
	"go.uber.org/zap"
	"golang.org/x/oauth2"

	"k8c.io/machine-controller/pkg/cloudprovider/common/ssh"
	cloudprovidererrors "k8c.io/machine-controller/pkg/cloudprovider/errors"
	"k8c.io/machine-controller/pkg/cloudprovider/instance"
	cloudprovidertypes "k8c.io/machine-controller/pkg/cloudprovider/types"
	common "k8c.io/machine-controller/sdk/apis/cluster/common"
	clusterv1alpha1 "k8c.io/machine-controller/sdk/apis/cluster/v1alpha1"
	linodetypes "k8c.io/machine-controller/sdk/cloudprovider/linode"
	"k8c.io/machine-controller/sdk/providerconfig"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/sets"
)

type provider struct {
	configVarResolver providerconfig.ConfigVarResolver
}

// New returns a linode provider.
func New(configVarResolver providerconfig.ConfigVarResolver) cloudprovidertypes.Provider {
	return &provider{configVarResolver: configVarResolver}
}

type Config struct {
	Token             string
	Region            string
	Type              string
	Backups           bool
	PrivateNetworking bool
	Tags              []string
}

const (
	createCheckTimeout     = 5 * time.Minute
	cloudinitStackScriptID = 392559
)

type TokenSource struct {
	AccessToken string
}

func (t *TokenSource) Token() (*oauth2.Token, error) {
	token := &oauth2.Token{
		AccessToken: t.AccessToken,
	}
	return token, nil
}

func getSlugForOS(os providerconfig.OperatingSystem) (string, error) {
	switch os {
	case providerconfig.OperatingSystemUbuntu:
		return "linode/ubuntu18.04", nil
	}
	return "", providerconfig.ErrOSNotSupported
}

func getClient(ctx context.Context, token string) linodego.Client {
	tokenSource := &TokenSource{
		AccessToken: token,
	}

	oauthClient := oauth2.NewClient(ctx, tokenSource)

	client := linodego.NewClient(oauthClient)
	client.SetUserAgent("Kubermatic linodego")

	return client
}

func (p *provider) getConfig(provSpec clusterv1alpha1.ProviderSpec) (*Config, *providerconfig.Config, error) {
	pconfig, err := providerconfig.GetConfig(provSpec)
	if err != nil {
		return nil, nil, err
	}

	if pconfig.OperatingSystemSpec.Raw == nil {
		return nil, nil, errors.New("operatingSystemSpec in the MachineDeployment cannot be empty")
	}

	rawConfig, err := linodetypes.GetConfig(*pconfig)
	if err != nil {
		return nil, nil, err
	}

	c := Config{}
	c.Token, err = p.configVarResolver.GetStringValueOrEnv(rawConfig.Token, "LINODE_TOKEN")
	if err != nil {
		return nil, nil, fmt.Errorf("failed to get the value of \"token\" field, error = %w", err)
	}
	c.Region, err = p.configVarResolver.GetStringValue(rawConfig.Region)
	if err != nil {
		return nil, nil, err
	}
	c.Type, err = p.configVarResolver.GetStringValue(rawConfig.Type)
	if err != nil {
		return nil, nil, err
	}
	c.Backups, _, err = p.configVarResolver.GetBoolValue(rawConfig.Backups)
	if err != nil {
		return nil, nil, err
	}
	c.PrivateNetworking, _, err = p.configVarResolver.GetBoolValue(rawConfig.PrivateNetworking)
	if err != nil {
		return nil, nil, err
	}

	for _, tag := range rawConfig.Tags {
		tagVal, err := p.configVarResolver.GetStringValue(tag)
		if err != nil {
			return nil, nil, err
		}
		c.Tags = append(c.Tags, tagVal)
	}

	return &c, pconfig, err
}

func (p *provider) AddDefaults(_ *zap.SugaredLogger, spec clusterv1alpha1.MachineSpec) (clusterv1alpha1.MachineSpec, error) {
	return spec, nil
}

func (p *provider) Validate(ctx context.Context, _ *zap.SugaredLogger, spec clusterv1alpha1.MachineSpec) error {
	c, pc, err := p.getConfig(spec.ProviderSpec)
	if err != nil {
		return fmt.Errorf("failed to parse config: %w", err)
	}

	if c.Token == "" {
		return errors.New("token is missing")
	}

	if c.Region == "" {
		return errors.New("region is missing")
	}

	if c.Type == "" {
		return errors.New("type is missing")
	}

	_, err = getSlugForOS(pc.OperatingSystem)
	if err != nil {
		return fmt.Errorf("invalid operating system specified %q: %w", pc.OperatingSystem, err)
	}

	client := getClient(ctx, c.Token)

	_, err = client.GetRegion(ctx, c.Region)
	if err != nil {
		return err
	}

	_, err = client.GetType(ctx, c.Type)
	if err != nil {
		return err
	}

	return nil
}

func createRandomPassword() (string, error) {
	rawRootPass := make([]byte, 50)
	_, err := rand.Read(rawRootPass)
	if err != nil {
		return "", fmt.Errorf("failed to generate random password: %w", err)
	}
	rootPass := base64.StdEncoding.EncodeToString(rawRootPass)
	return rootPass, nil
}

func (p *provider) Create(ctx context.Context, _ *zap.SugaredLogger, machine *clusterv1alpha1.Machine, _ *cloudprovidertypes.ProviderData, userdata string) (instance.Instance, error) {
	c, pc, err := p.getConfig(machine.Spec.ProviderSpec)
	if err != nil {
		return nil, cloudprovidererrors.TerminalError{
			Reason:  common.InvalidConfigurationMachineError,
			Message: fmt.Sprintf("Failed to parse MachineSpec, due to %v", err),
		}
	}

	client := getClient(ctx, c.Token)

	sshkey, err := ssh.NewKey()
	if err != nil {
		return nil, fmt.Errorf("failed to generate ssh key: %w", err)
	}

	slug, err := getSlugForOS(pc.OperatingSystem)
	if err != nil {
		return nil, cloudprovidererrors.TerminalError{
			Reason:  common.InvalidConfigurationMachineError,
			Message: fmt.Sprintf("Failed to parse MachineSpec, invalid operating system specified %q: %v", pc.OperatingSystem, err),
		}
	}

	randomPassword, err := createRandomPassword()
	if err != nil {
		return nil, cloudprovidererrors.TerminalError{
			Reason:  common.InvalidConfigurationMachineError,
			Message: fmt.Sprintf("Failed to generate password for Linode, due to %v", err),
		}
	}

	createRequest := linodego.InstanceCreateOptions{
		Image:          slug,
		Label:          fmt.Sprintf("%.32s", machine.Spec.Name),
		Region:         c.Region,
		Type:           c.Type,
		PrivateIP:      c.PrivateNetworking,
		RootPass:       randomPassword,
		BackupsEnabled: c.Backups,
		AuthorizedKeys: []string{strings.TrimSpace(sshkey.PublicKey)},
		Tags:           append(c.Tags, string(machine.UID)),
		StackScriptID:  cloudinitStackScriptID,
		StackScriptData: map[string]string{
			"userdata": base64.StdEncoding.EncodeToString([]byte(userdata)),
		},
	}

	linode, err := client.CreateInstance(ctx, createRequest)
	if err != nil {
		return nil, linodeStatusAndErrToTerminalError(err)
	}

	linode, err = client.WaitForInstanceStatus(ctx, linode.ID, linodego.InstanceRunning, int(createCheckTimeout/time.Second))
	if err != nil {
		return nil, linodeStatusAndErrToTerminalError(err)
	}

	return &linodeInstance{linode: linode}, err
}

func (p *provider) Cleanup(ctx context.Context, log *zap.SugaredLogger, machine *clusterv1alpha1.Machine, data *cloudprovidertypes.ProviderData) (bool, error) {
	instance, err := p.Get(ctx, log, machine, data)
	if err != nil {
		if errors.Is(err, cloudprovidererrors.ErrInstanceNotFound) {
			return true, nil
		}
		return false, err
	}

	c, _, err := p.getConfig(machine.Spec.ProviderSpec)
	if err != nil {
		return false, cloudprovidererrors.TerminalError{
			Reason:  common.InvalidConfigurationMachineError,
			Message: fmt.Sprintf("Failed to parse MachineSpec, due to %v", err),
		}
	}
	client := getClient(ctx, c.Token)

	linodeID, err := strconv.Atoi(instance.ID())
	if err != nil {
		return false, fmt.Errorf("failed to convert instance id %s to int: %w", instance.ID(), err)
	}

	err = client.DeleteInstance(ctx, linodeID)
	if err != nil {
		return false, linodeStatusAndErrToTerminalError(err)
	}

	return false, nil
}

func getListOptions(name string) *linodego.ListOptions {
	filter, _ := json.Marshal(map[string]interface{}{
		"label": fmt.Sprintf("%.32s", name),
	})

	listOptions := linodego.NewListOptions(0, string(filter))
	return listOptions
}

func (p *provider) Get(ctx context.Context, _ *zap.SugaredLogger, machine *clusterv1alpha1.Machine, _ *cloudprovidertypes.ProviderData) (instance.Instance, error) {
	c, _, err := p.getConfig(machine.Spec.ProviderSpec)
	if err != nil {
		return nil, cloudprovidererrors.TerminalError{
			Reason:  common.InvalidConfigurationMachineError,
			Message: fmt.Sprintf("Failed to parse MachineSpec, due to %v", err),
		}
	}

	client := getClient(ctx, c.Token)

	listOptions := getListOptions(machine.Spec.Name)
	linodes, err := client.ListInstances(ctx, listOptions)

	if err != nil {
		return nil, linodeStatusAndErrToTerminalError(err)
	}

	for i, linode := range linodes {
		if sets.NewString(linode.Tags...).Has(string(machine.UID)) {
			return &linodeInstance{linode: &linodes[i]}, nil
		}
	}

	return nil, cloudprovidererrors.ErrInstanceNotFound
}

func (p *provider) MigrateUID(ctx context.Context, _ *zap.SugaredLogger, machine *clusterv1alpha1.Machine, newUID types.UID) error {
	c, _, err := p.getConfig(machine.Spec.ProviderSpec)
	if err != nil {
		return fmt.Errorf("failed to decode providerconfig: %w", err)
	}
	client := getClient(ctx, c.Token)
	listOptions := getListOptions(machine.Spec.Name)
	linodes, err := client.ListInstances(ctx, listOptions)
	if err != nil {
		return fmt.Errorf("failed to list linodes: %w", err)
	}

	for _, linode := range linodes {
		if sets.NewString(linode.Tags...).Has(string(machine.UID)) {
			updateOpts := linode.GetUpdateOptions()

			tags := []string{string(newUID)}
			if updateOpts.Tags != nil {
				oldUID := string(machine.UID)
				for _, existingTag := range *updateOpts.Tags {
					if existingTag != oldUID {
						tags = append(tags, existingTag)
					}
				}
			}
			updateOpts.Tags = &tags
			_, err = client.UpdateInstance(ctx, linode.ID, updateOpts)
			if err != nil {
				return fmt.Errorf("failed to revise linode UID tags: %w", err)
			}
		}
	}

	return nil
}

func (p *provider) MachineMetricsLabels(machine *clusterv1alpha1.Machine) (map[string]string, error) {
	labels := make(map[string]string)

	c, _, err := p.getConfig(machine.Spec.ProviderSpec)
	if err == nil {
		labels["type"] = c.Type
		labels["region"] = c.Region
	}

	return labels, err
}

type linodeInstance struct {
	linode *linodego.Instance
}

func (d *linodeInstance) Name() string {
	return d.linode.Label
}

func (d *linodeInstance) ID() string {
	return strconv.Itoa(d.linode.ID)
}

func (d *linodeInstance) ProviderID() string {
	if d == nil || d.ID() == "" {
		return ""
	}
	return fmt.Sprintf("linode://%s", d.ID())
}

func (d *linodeInstance) Addresses() map[string]corev1.NodeAddressType {
	addresses := map[string]corev1.NodeAddressType{}
	for _, n := range d.linode.IPv4 {
		addresses[n.String()] = corev1.NodeInternalIP
	}
	addresses[d.linode.IPv6] = corev1.NodeInternalIP
	return addresses
}

func (d *linodeInstance) Status() instance.Status {
	switch d.linode.Status {
	case linodego.InstanceProvisioning, linodego.InstanceBooting:
		return instance.StatusCreating
	case linodego.InstanceRunning:
		return instance.StatusRunning
	case linodego.InstanceDeleting:
		return instance.StatusDeleting
	default:
		// Cloning, Migrating, Offline, Rebooting,
		// Rebuilding, Resizing, Restoring, ShuttingDown
		return instance.StatusUnknown
	}
}

// linodeStatusAndErrToTerminalError judges if the given HTTP status
// can be qualified as a "terminal" error, for more info see v1alpha1.MachineStatus

// if the given error doesn't qualify the error passed as
// an argument will be returned.
func linodeStatusAndErrToTerminalError(err error) error {
	status := 0
	var apiErr *linodego.Error
	if errors.As(err, &apiErr) {
		status = apiErr.Code
	}

	switch status {
	case http.StatusUnauthorized:
		// authorization primitives come from MachineSpec
		// thus we are setting InvalidConfigurationMachineError
		return cloudprovidererrors.TerminalError{
			Reason:  common.InvalidConfigurationMachineError,
			Message: "A request has been rejected due to invalid credentials which were taken from the MachineSpec",
		}
	default:
		return err
	}
}

func (p *provider) SetMetricsForMachines(_ clusterv1alpha1.MachineList) error {
	return nil
}
