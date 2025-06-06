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

package digitalocean

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"time"

	"github.com/digitalocean/godo"
	"go.uber.org/zap"
	"golang.org/x/oauth2"

	"k8c.io/machine-controller/pkg/cloudprovider/common/ssh"
	cloudprovidererrors "k8c.io/machine-controller/pkg/cloudprovider/errors"
	"k8c.io/machine-controller/pkg/cloudprovider/instance"
	cloudprovidertypes "k8c.io/machine-controller/pkg/cloudprovider/types"
	"k8c.io/machine-controller/sdk/apis/cluster/common"
	clusterv1alpha1 "k8c.io/machine-controller/sdk/apis/cluster/v1alpha1"
	digitaloceantypes "k8c.io/machine-controller/sdk/cloudprovider/digitalocean"
	"k8c.io/machine-controller/sdk/net"
	"k8c.io/machine-controller/sdk/providerconfig"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/sets"
	"k8s.io/apimachinery/pkg/util/wait"
)

type provider struct {
	configVarResolver providerconfig.ConfigVarResolver
}

// New returns a digitalocean provider.
func New(configVarResolver providerconfig.ConfigVarResolver) cloudprovidertypes.Provider {
	return &provider{configVarResolver: configVarResolver}
}

type Config struct {
	Token             string
	Region            string
	Size              string
	Backups           bool
	IPv6              bool
	PrivateNetworking bool
	Monitoring        bool
	Tags              []string
}

const (
	createCheckPeriod           = 10 * time.Second
	createCheckTimeout          = 5 * time.Minute
	createCheckFailedWaitPeriod = 10 * time.Second
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
		return "ubuntu-24-04-x64", nil
	case providerconfig.OperatingSystemRockyLinux:
		return "rockylinux-8-x64", nil
	}
	return "", providerconfig.ErrOSNotSupported
}

func getClient(ctx context.Context, token string) *godo.Client {
	tokenSource := &TokenSource{
		AccessToken: token,
	}

	oauthClient := oauth2.NewClient(ctx, tokenSource)
	return godo.NewClient(oauthClient)
}

func (p *provider) getConfig(provSpec clusterv1alpha1.ProviderSpec) (*Config, *providerconfig.Config, error) {
	pconfig, err := providerconfig.GetConfig(provSpec)
	if err != nil {
		return nil, nil, err
	}

	if pconfig.OperatingSystemSpec.Raw == nil {
		return nil, nil, errors.New("operatingSystemSpec in the MachineDeployment cannot be empty")
	}

	rawConfig, err := digitaloceantypes.GetConfig(*pconfig)
	if err != nil {
		return nil, nil, err
	}

	c := Config{}
	c.Token, err = p.configVarResolver.GetStringValueOrEnv(rawConfig.Token, "DO_TOKEN")
	if err != nil {
		return nil, nil, fmt.Errorf("failed to get the value of \"token\" field, error = %w", err)
	}
	c.Region, err = p.configVarResolver.GetStringValue(rawConfig.Region)
	if err != nil {
		return nil, nil, err
	}
	c.Size, err = p.configVarResolver.GetStringValue(rawConfig.Size)
	if err != nil {
		return nil, nil, err
	}
	c.Backups, _, err = p.configVarResolver.GetBoolValue(rawConfig.Backups)
	if err != nil {
		return nil, nil, err
	}
	c.IPv6, _, err = p.configVarResolver.GetBoolValue(rawConfig.IPv6)
	if err != nil {
		return nil, nil, err
	}
	c.PrivateNetworking, _, err = p.configVarResolver.GetBoolValue(rawConfig.PrivateNetworking)
	if err != nil {
		return nil, nil, err
	}
	c.Monitoring, _, err = p.configVarResolver.GetBoolValue(rawConfig.Monitoring)
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

	if c.Size == "" {
		return errors.New("size is missing")
	}

	_, err = getSlugForOS(pc.OperatingSystem)
	if err != nil {
		return fmt.Errorf("invalid operating system specified %q: %w", pc.OperatingSystem, err)
	}

	switch f := pc.Network.GetIPFamily(); f {
	case net.IPFamilyUnspecified, net.IPFamilyIPv4:
	// noop
	case net.IPFamilyIPv6:
		return fmt.Errorf(net.ErrIPv6OnlyUnsupported)
	case net.IPFamilyIPv4IPv6, net.IPFamilyIPv6IPv4:
		// noop
	default:
		return fmt.Errorf(net.ErrUnknownNetworkFamily, f)
	}

	client := getClient(ctx, c.Token)

	regions, _, err := client.Regions.List(ctx, &godo.ListOptions{PerPage: 1000})
	if err != nil {
		return err
	}
	var foundRegion bool
	for _, region := range regions {
		if region.Slug == c.Region {
			foundRegion = true
			break
		}
	}
	if !foundRegion {
		return fmt.Errorf("region %q not found", c.Region)
	}

	sizes, _, err := client.Sizes.List(ctx, &godo.ListOptions{PerPage: 1000})
	if err != nil {
		return err
	}
	var foundSize bool
	for _, size := range sizes {
		if size.Slug == c.Size {
			if !size.Available {
				return fmt.Errorf("size is not available")
			}

			var regionAvailable bool
			for _, region := range size.Regions {
				if region == c.Region {
					regionAvailable = true
					break
				}
			}

			if !regionAvailable {
				return fmt.Errorf("size %q is not available in region %q", c.Size, c.Region)
			}

			foundSize = true
			break
		}
	}
	if !foundSize {
		return fmt.Errorf("size %q not found", c.Size)
	}

	return nil
}

// uploadRandomSSHPublicKey generates a random key pair and uploads the public part of the key to
// digital ocean because it is not possible to create a droplet without ssh key assigned
// this method returns an error if the key already exists.
func uploadRandomSSHPublicKey(ctx context.Context, service godo.KeysService) (string, error) {
	sshkey, err := ssh.NewKey()
	if err != nil {
		return "", fmt.Errorf("failed to generate ssh key: %w", err)
	}

	existingkey, res, err := service.GetByFingerprint(ctx, sshkey.FingerprintMD5)
	if err == nil && existingkey != nil && res.StatusCode >= http.StatusOK && res.StatusCode <= http.StatusAccepted {
		return "", fmt.Errorf("failed to create ssh public key, the key already exists")
	}

	newDoKey, rsp, err := service.Create(ctx, &godo.KeyCreateRequest{
		PublicKey: sshkey.PublicKey,
		Name:      sshkey.Name,
	})
	if err != nil {
		return "", doStatusAndErrToTerminalError(rsp.StatusCode, fmt.Errorf("failed to create ssh public key on digitalocean: %w", err))
	}

	return newDoKey.Fingerprint, nil
}

func (p *provider) Create(ctx context.Context, log *zap.SugaredLogger, machine *clusterv1alpha1.Machine, _ *cloudprovidertypes.ProviderData, userdata string) (instance.Instance, error) {
	c, pc, err := p.getConfig(machine.Spec.ProviderSpec)
	if err != nil {
		return nil, cloudprovidererrors.TerminalError{
			Reason:  common.InvalidConfigurationMachineError,
			Message: fmt.Sprintf("Failed to parse MachineSpec, due to %v", err),
		}
	}

	client := getClient(ctx, c.Token)

	fingerprint, err := uploadRandomSSHPublicKey(ctx, client.Keys)
	if err != nil {
		return nil, err
	}
	defer func() {
		_, err := client.Keys.DeleteByFingerprint(ctx, fingerprint)
		if err != nil {
			log.Errorw("Failed to remove a temporary ssh key", "fingerprint", fingerprint, zap.Error(err))
		}
	}()

	slug, err := getSlugForOS(pc.OperatingSystem)
	if err != nil {
		return nil, cloudprovidererrors.TerminalError{
			Reason:  common.InvalidConfigurationMachineError,
			Message: fmt.Sprintf("Failed to parse MachineSpec, invalid operating system specified %q: %v", pc.OperatingSystem, err),
		}
	}
	createRequest := &godo.DropletCreateRequest{
		Image:             godo.DropletCreateImage{Slug: slug},
		Name:              machine.Spec.Name,
		Region:            c.Region,
		Size:              c.Size,
		IPv6:              c.IPv6 || pc.Network.GetIPFamily().IsDualstack(),
		PrivateNetworking: c.PrivateNetworking,
		Backups:           c.Backups,
		Monitoring:        c.Monitoring,
		UserData:          userdata,
		SSHKeys:           []godo.DropletCreateSSHKey{{Fingerprint: fingerprint}},
		Tags:              append(c.Tags, string(machine.UID)),
	}

	droplet, rsp, err := client.Droplets.Create(ctx, createRequest)
	if err != nil {
		return nil, doStatusAndErrToTerminalError(rsp.StatusCode, err)
	}

	dropletLog := log.With("droplet", droplet.ID)

	//We need to wait until the droplet really got created as tags will be only applied when the droplet is running
	err = wait.PollUntilContextTimeout(ctx, createCheckPeriod, createCheckTimeout, false, func(ctx context.Context) (bool, error) {
		newDroplet, rsp, err := client.Droplets.Get(ctx, droplet.ID)
		if err != nil {
			tErr := doStatusAndErrToTerminalError(rsp.StatusCode, err)
			if isTerminalError, _, _ := cloudprovidererrors.IsTerminalError(tErr); isTerminalError {
				return true, tErr
			}
			//Well just wait 10 sec and hope the droplet got started by then...
			time.Sleep(createCheckFailedWaitPeriod)
			return false, fmt.Errorf("droplet %q got created but we failed to fetch its status", droplet.ID)
		}
		if sets.NewString(newDroplet.Tags...).Has(string(machine.UID)) {
			dropletLog.Debug("Droplet got fully created")
			return true, nil
		}
		dropletLog.Debug("Waiting until droplet got fully created...")
		return false, nil
	})

	return &doInstance{droplet: droplet}, err
}

func (p *provider) Cleanup(ctx context.Context, _ *zap.SugaredLogger, machine *clusterv1alpha1.Machine, _ *cloudprovidertypes.ProviderData) (bool, error) {
	instance, err := p.get(ctx, machine)
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

	doID, err := strconv.Atoi(instance.ID())
	if err != nil {
		return false, fmt.Errorf("failed to convert instance id %s to int: %w", instance.ID(), err)
	}

	rsp, err := client.Droplets.Delete(ctx, doID)
	if err != nil {
		return false, doStatusAndErrToTerminalError(rsp.StatusCode, err)
	}

	return false, nil
}

func (p *provider) Get(ctx context.Context, _ *zap.SugaredLogger, machine *clusterv1alpha1.Machine, _ *cloudprovidertypes.ProviderData) (instance.Instance, error) {
	return p.get(ctx, machine)
}

func (p *provider) get(ctx context.Context, machine *clusterv1alpha1.Machine) (*doInstance, error) {
	c, _, err := p.getConfig(machine.Spec.ProviderSpec)
	if err != nil {
		return nil, cloudprovidererrors.TerminalError{
			Reason:  common.InvalidConfigurationMachineError,
			Message: fmt.Sprintf("Failed to parse MachineSpec, due to %v", err),
		}
	}

	droplets, err := p.listDroplets(ctx, c.Token)
	if err != nil {
		return nil, err
	}

	for i, droplet := range droplets {
		if droplet.Name == machine.Spec.Name && sets.NewString(droplet.Tags...).Has(string(machine.UID)) {
			return &doInstance{droplet: &droplets[i]}, nil
		}
	}

	return nil, cloudprovidererrors.ErrInstanceNotFound
}

func (p *provider) listDroplets(ctx context.Context, token string) ([]godo.Droplet, error) {
	client := getClient(ctx, token)
	result := make([]godo.Droplet, 0)

	opt := &godo.ListOptions{
		PerPage: 200,
	}

	for {
		droplets, resp, err := client.Droplets.List(ctx, opt)
		if err != nil {
			return nil, doStatusAndErrToTerminalError(resp.StatusCode, fmt.Errorf("failed to get droplets: %w", err))
		}

		result = append(result, droplets...)

		if resp.Links == nil || resp.Links.IsLastPage() {
			break
		}

		page, err := resp.Links.CurrentPage()
		if err != nil {
			return nil, err
		}

		opt.Page = page + 1
	}

	return result, nil
}

func (p *provider) MigrateUID(ctx context.Context, _ *zap.SugaredLogger, machine *clusterv1alpha1.Machine, newUID types.UID) error {
	c, _, err := p.getConfig(machine.Spec.ProviderSpec)
	if err != nil {
		return fmt.Errorf("failed to decode providerconfig: %w", err)
	}
	client := getClient(ctx, c.Token)
	droplets, _, err := client.Droplets.List(ctx, &godo.ListOptions{PerPage: 1000})
	if err != nil {
		return fmt.Errorf("failed to list droplets: %w", err)
	}

	// The create does not fail if that tag already exists, it even keep responding with a http/201
	_, response, err := client.Tags.Create(ctx, &godo.TagCreateRequest{Name: string(newUID)})
	if err != nil {
		return fmt.Errorf("failed to create new UID tag: %w, status code: %v", err, response.StatusCode)
	}

	for _, droplet := range droplets {
		if droplet.Name == machine.Spec.Name && sets.NewString(droplet.Tags...).Has(string(machine.UID)) {
			tagResourceRequest := &godo.TagResourcesRequest{
				Resources: []godo.Resource{{ID: strconv.Itoa(droplet.ID), Type: godo.DropletResourceType}},
			}
			_, err = client.Tags.TagResources(ctx, string(newUID), tagResourceRequest)
			if err != nil {
				return fmt.Errorf("failed to tag droplet with new UID tag: %w", err)
			}
			untagResourceRequest := &godo.UntagResourcesRequest{
				Resources: []godo.Resource{{ID: strconv.Itoa(droplet.ID), Type: godo.DropletResourceType}},
			}
			_, err = client.Tags.UntagResources(ctx, string(machine.UID), untagResourceRequest)
			if err != nil {
				return fmt.Errorf("failed to remove old UID tag: %w", err)
			}
		}
	}

	return nil
}

func (p *provider) MachineMetricsLabels(machine *clusterv1alpha1.Machine) (map[string]string, error) {
	labels := make(map[string]string)

	c, _, err := p.getConfig(machine.Spec.ProviderSpec)
	if err == nil {
		labels["size"] = c.Size
		labels["region"] = c.Region
	}

	return labels, err
}

type doInstance struct {
	droplet *godo.Droplet
}

func (d *doInstance) Name() string {
	return d.droplet.Name
}

func (d *doInstance) ID() string {
	return strconv.Itoa(d.droplet.ID)
}

func (d *doInstance) ProviderID() string {
	if d.droplet == nil || d.droplet.Name == "" {
		return ""
	}
	return fmt.Sprintf("digitalocean://%d", d.droplet.ID)
}

func (d *doInstance) Addresses() map[string]corev1.NodeAddressType {
	addresses := map[string]corev1.NodeAddressType{}
	for _, n := range d.droplet.Networks.V4 {
		if n.Type == "public" {
			addresses[n.IPAddress] = corev1.NodeExternalIP
		} else {
			addresses[n.IPAddress] = corev1.NodeInternalIP
		}
	}
	for _, n := range d.droplet.Networks.V6 {
		if n.Type == "public" {
			addresses[n.IPAddress] = corev1.NodeExternalIP
		} else {
			addresses[n.IPAddress] = corev1.NodeInternalIP
		}
	}
	return addresses
}

func (d *doInstance) Status() instance.Status {
	switch d.droplet.Status {
	case "new":
		return instance.StatusCreating
	case "active":
		return instance.StatusRunning
	default:
		return instance.StatusUnknown
	}
}

// doStatusAndErrToTerminalError judges if the given HTTP status
// can be qualified as a "terminal" error, for more info see v1alpha1.MachineStatus

// if the given error doesn't qualify the error passed as
// an argument will be returned.
func doStatusAndErrToTerminalError(status int, err error) error {
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
