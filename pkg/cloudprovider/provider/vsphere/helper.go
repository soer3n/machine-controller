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

package vsphere

import (
	"bytes"
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"text/template"

	"github.com/vmware/govmomi/object"
	"github.com/vmware/govmomi/vapi/tags"
	"github.com/vmware/govmomi/vim25/mo"
	"github.com/vmware/govmomi/vim25/soap"
	"github.com/vmware/govmomi/vim25/types"
	"go.uber.org/zap"

	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
)

const (
	localTempDir     = "/tmp"
	metaDataTemplate = `instance-id: {{ .InstanceID}}
local-hostname: {{ .Hostname }}`

	gigaByte = (1024 * 1024 * 1024)
)

func createClonedVM(ctx context.Context, log *zap.SugaredLogger, vmName string, config *Config, session *Session, containerLinuxUserdata string) (*object.VirtualMachine, error) {
	tpl, err := session.Finder.VirtualMachine(ctx, config.TemplateVMName)
	if err != nil {
		return nil, fmt.Errorf("failed to get template vm: %w", err)
	}

	// Find the target folder, if its included in the provider config.
	var targetVMFolder *object.Folder
	if config.Folder != "" {
		// If non-absolute folder name is used, e.g. 'duplicate-folder' it can match
		// multiple folders and thus fail. It will also gladly match a folder from
		// a different datacenter. It is therefore preferable to use absolute folder
		// paths, e.g. '/Datacenter/vm/nested/folder'.
		// The target folder must already exist.
		targetVMFolder, err = session.Finder.Folder(ctx, config.Folder)
		if err != nil {
			return nil, fmt.Errorf("failed to get target folder: %w", err)
		}
	} else {
		// Do not query datacenter folders unless required
		datacenterFolders, err := session.Datacenter.Folders(ctx)
		if err != nil {
			return nil, fmt.Errorf("failed to get datacenter folders: %w", err)
		}
		targetVMFolder = datacenterFolders.VmFolder
	}

	relocateSpec := types.VirtualMachineRelocateSpec{
		DiskMoveType: string(types.VirtualMachineRelocateDiskMoveOptionsMoveAllDiskBackingsAndConsolidate),
		Folder:       types.NewReference(targetVMFolder.Reference()),
		Disk:         []types.VirtualMachineRelocateSpecDiskLocator{},
	}
	cloneSpec := types.VirtualMachineCloneSpec{
		PowerOn:  false,
		Template: false,
		Location: relocateSpec,
	}
	datastoreref, err := resolveDatastoreRef(ctx, log, config, session, tpl, targetVMFolder, &cloneSpec)
	if err != nil {
		return nil, fmt.Errorf("failed to resolve datastore: %w", err)
	}

	resourcepoolref, err := resolveResourcePoolRef(ctx, config, session)
	if err != nil {
		return nil, fmt.Errorf("failed to resolve resourcePool: %w", err)
	}

	cloneSpec.Location.Datastore = datastoreref
	cloneSpec.Location.Pool = resourcepoolref
	// Create a cloned VM from the template VM's snapshot.
	// We split the cloning from the reconfiguring as those actions differ on the permission side.
	// It's nicer to tell which specific action failed due to lacking permissions.
	clonedVMTask, err := tpl.Clone(ctx, targetVMFolder, vmName, cloneSpec)
	if err != nil {
		return nil, fmt.Errorf("failed to clone template vm: %w", err)
	}

	if err := clonedVMTask.WaitEx(ctx); err != nil {
		return nil, fmt.Errorf("error when waiting for result of clone task: %w", err)
	}
	virtualMachine, err := session.Finder.VirtualMachine(ctx, vmName)
	if err != nil {
		return nil, fmt.Errorf("failed to get virtual machine object after cloning: %w", err)
	}
	vmDevices, err := virtualMachine.Device(ctx)
	if err != nil {
		return nil, fmt.Errorf("failed to list devices of template VM: %w", err)
	}

	var vAppAconfig *types.VmConfigSpec
	if containerLinuxUserdata != "" {
		userdataBase64 := base64.StdEncoding.EncodeToString([]byte(containerLinuxUserdata))

		// The properties describing userdata will already exist in the Flatcar VM template.
		// In order to overwrite them, we need to specify their numeric Key values,
		// which we'll extract from that template.
		var mvm mo.VirtualMachine
		if err := virtualMachine.Properties(ctx, virtualMachine.Reference(), []string{"config", "config.vAppConfig", "config.vAppConfig.property"}, &mvm); err != nil {
			return nil, fmt.Errorf("failed to extract vapp properties for flatcar: %w", err)
		}

		var propertySpecs []types.VAppPropertySpec
		if mvm.Config.VAppConfig.GetVmConfigInfo() == nil {
			return nil, fmt.Errorf("no vm config found in template '%s'. Make sure you import the correct OVA with the appropriate flatcar settings", config.TemplateVMName)
		}

		var (
			guestInfoUserData         string
			guestInfoUserDataEncoding string
		)

		guestInfoUserData = "guestinfo.ignition.config.data"
		guestInfoUserDataEncoding = "guestinfo.ignition.config.data.encoding"
		for _, item := range mvm.Config.VAppConfig.GetVmConfigInfo().Property {
			switch item.Id {
			case guestInfoUserData:
				propertySpecs = append(propertySpecs, types.VAppPropertySpec{
					ArrayUpdateSpec: types.ArrayUpdateSpec{
						Operation: types.ArrayUpdateOperationEdit,
					},
					Info: &types.VAppPropertyInfo{
						Key:   item.Key,
						Id:    item.Id,
						Value: userdataBase64,
					},
				})
			case guestInfoUserDataEncoding:
				propertySpecs = append(propertySpecs, types.VAppPropertySpec{
					ArrayUpdateSpec: types.ArrayUpdateSpec{
						Operation: types.ArrayUpdateOperationEdit,
					},
					Info: &types.VAppPropertyInfo{
						Key:   item.Key,
						Id:    item.Id,
						Value: "base64",
					},
				})
			}
		}

		vAppAconfig = &types.VmConfigSpec{Property: propertySpecs}
	}

	diskUUIDEnabled := true
	var deviceSpecs []types.BaseVirtualDeviceConfigSpec
	if config.DiskSizeGB != nil {
		disks, err := getDisksFromVM(ctx, virtualMachine)
		if err != nil {
			return nil, fmt.Errorf("failed to get disks from VM: %w", err)
		}
		// If this is wrong, the resulting error is `Invalid operation for device '0`
		// so verify again this is legit
		if err := validateDiskResizing(disks, *config.DiskSizeGB); err != nil {
			return nil, err
		}

		log.Debugw("Increasing disk size", "targetgb", *config.DiskSizeGB)
		disk := disks[0]
		disk.CapacityInBytes = *config.DiskSizeGB * gigaByte
		diskspec := &types.VirtualDeviceConfigSpec{Operation: types.VirtualDeviceConfigSpecOperationEdit, Device: disk}
		deviceSpecs = append(deviceSpecs, diskspec)
	}

	if config.VMNetName != "" || len(config.Networks) > 0 {
		networkSpecs, err := GetNetworkSpecs(ctx, session, vmDevices, config.VMNetName, config.Networks)
		if err != nil {
			return nil, fmt.Errorf("failed to get network specifications: %w", err)
		}
		deviceSpecs = append(deviceSpecs, networkSpecs...)
	}

	vmConfig := types.VirtualMachineConfigSpec{
		DeviceChange: deviceSpecs,
		Flags: &types.VirtualMachineFlagInfo{
			DiskUuidEnabled: &diskUUIDEnabled,
		},
		NumCPUs:    config.CPUs,
		MemoryMB:   config.MemoryMB,
		VAppConfig: vAppAconfig,
	}
	reconfigureTask, err := virtualMachine.Reconfigure(ctx, vmConfig)
	if err != nil {
		return nil, fmt.Errorf("failed to reconfigure the VM: %w", err)
	}
	if err := reconfigureTask.WaitEx(ctx); err != nil {
		return nil, fmt.Errorf("error when waiting for result of the reconfigure task: %w", err)
	}

	// Ubuntu won't boot with attached floppy device, because it tries to write to it
	// which fails, because the floppy device does not contain a floppy disk
	// Upstream issue: https://bugs.launchpad.net/cloud-images/+bug/1573095
	if err := removeFloppyDevice(ctx, virtualMachine); err != nil {
		return nil, fmt.Errorf("failed to remove floppy device: %w", err)
	}
	return virtualMachine, nil
}

func resolveDatastoreRef(ctx context.Context, log *zap.SugaredLogger, config *Config, session *Session, vm *object.VirtualMachine, folder *object.Folder, cloneSpec *types.VirtualMachineCloneSpec) (*types.ManagedObjectReference, error) {
	// Based on https://github.com/vmware/govmomi/blob/v0.22.1/govc/vm/clone.go#L358
	if config.DatastoreCluster != "" && config.Datastore == "" {
		vmLog := log.With("vm", vm.Name(), "datastorecluster", config.DatastoreCluster)
		vmLog.Infow("Choosing initial datastore placement for vm from datastore cluster")

		storagePod, err := session.Finder.DatastoreCluster(ctx, config.DatastoreCluster)
		if err != nil {
			return nil, fmt.Errorf("failed to get datastore cluster: %w", err)
		}

		// Build pod selection spec from config spec
		podSelectionSpec := types.StorageDrsPodSelectionSpec{
			StoragePod: types.NewReference(storagePod.Reference()),
		}

		// Get the virtual machine reference
		vmref := vm.Reference()

		// TODO(irozzo) moveAllDiskBackingsAndConsolidate does not seem to work with RecommendDatastore,
		// try to better understand the reason and the implications.
		// https://code.vmware.com/docs/4206/vsphere-web-services-api-reference/doc/vim.vm.RelocateSpec.DiskMoveOptions.html
		cloneSpec.Location.DiskMoveType = string(types.VirtualMachineRelocateDiskMoveOptionsMoveAllDiskBackingsAndDisallowSharing)
		// Build the placement spec
		storagePlacementSpec := types.StoragePlacementSpec{
			Folder:           types.NewReference(folder.Reference()),
			Vm:               &vmref,
			CloneName:        vm.Name(),
			CloneSpec:        cloneSpec,
			PodSelectionSpec: podSelectionSpec,
			Type:             string(types.StoragePlacementSpecPlacementTypeClone),
		}

		// Get the storage placement result
		storageResourceManager := object.NewStorageResourceManager(session.Client.Client)
		result, err := storageResourceManager.RecommendDatastores(ctx, storagePlacementSpec)
		if err != nil {
			return nil, fmt.Errorf("error occurred while getting storage placement recommendation: %w", err)
		}

		// Get the recommendations
		recommendations := result.Recommendations
		if len(recommendations) == 0 {
			return nil, fmt.Errorf("no recommendations")
		}

		// Get the first recommendation
		ds := recommendations[0].Action[0].(*types.StoragePlacementAction).Destination.Reference()
		vmLog.Infow("Selected datastore from datastore cluster", "datastore", ds)

		return &ds, nil
	} else if config.DatastoreCluster == "" && config.Datastore != "" {
		datastore, err := session.Finder.Datastore(ctx, config.Datastore)
		if err != nil {
			return nil, fmt.Errorf("failed to get datastore: %w", err)
		}
		return types.NewReference(datastore.Reference()), nil
	}
	return nil, fmt.Errorf("please provide either a datastore or a datastore cluster")
}

func uploadAndAttachISO(ctx context.Context, log *zap.SugaredLogger, session *Session, vmRef *object.VirtualMachine, localIsoFilePath string) error {
	p := soap.DefaultUpload
	remoteIsoFilePath := fmt.Sprintf("%s/%s", vmRef.Name(), "cloud-init.iso")
	// Get the datastore where VM files are located
	datastore, err := getDatastoreFromVM(ctx, session, vmRef)
	if err != nil {
		return fmt.Errorf("error getting datastore from VM %s: %w", vmRef.Name(), err)
	}
	uploadLog := log.With("datastore", datastore, "source", localIsoFilePath, "destination", remoteIsoFilePath)
	uploadLog.Debug("Uploading userdata ISO to datastore")
	if err := datastore.UploadFile(ctx, localIsoFilePath, remoteIsoFilePath, &p); err != nil {
		return fmt.Errorf("failed to upload iso: %w", err)
	}
	uploadLog.Debug("Uploaded ISO file")

	// Find the cd-rom device and insert the cloud init iso file into it.
	devices, err := vmRef.Device(ctx)
	if err != nil {
		return fmt.Errorf("failed to get devices: %w", err)
	}

	// passing empty cd-rom name so that the first one gets returned
	cdrom, err := devices.FindCdrom("")
	if err != nil {
		return fmt.Errorf("failed to find cdrom device: %w", err)
	}
	cdrom.Connectable.StartConnected = true
	iso := datastore.Path(remoteIsoFilePath)
	return vmRef.EditDevice(ctx, devices.InsertIso(cdrom, iso))
}

func generateLocalUserdataISO(userdata, name string) (string, error) {
	// We must create a directory, because the iso-generation commands
	// take a directory as input
	userdataDir, err := os.MkdirTemp(localTempDir, name)
	if err != nil {
		return "", fmt.Errorf("failed to create local temp directory for userdata at %s: %w", userdataDir, err)
	}
	defer func() {
		if err := os.RemoveAll(userdataDir); err != nil {
			utilruntime.HandleError(fmt.Errorf("error cleaning up local userdata tempdir %s: %w", userdataDir, err))
		}
	}()

	userdataFilePath := fmt.Sprintf("%s/user-data", userdataDir)
	metadataFilePath := fmt.Sprintf("%s/meta-data", userdataDir)
	isoFilePath := fmt.Sprintf("%s/%s.iso", localTempDir, name)

	metadataTmpl, err := template.New("metadata").Parse(metaDataTemplate)
	if err != nil {
		return "", fmt.Errorf("failed to parse metadata template: %w", err)
	}
	metadata := &bytes.Buffer{}
	templateContext := struct {
		InstanceID string
		Hostname   string
	}{
		InstanceID: name,
		Hostname:   name,
	}
	if err = metadataTmpl.Execute(metadata, templateContext); err != nil {
		return "", fmt.Errorf("failed to render metadata: %w", err)
	}

	if err := os.WriteFile(userdataFilePath, []byte(userdata), 0644); err != nil {
		return "", fmt.Errorf("failed to locally write userdata file to %s: %w", userdataFilePath, err)
	}

	if err := os.WriteFile(metadataFilePath, metadata.Bytes(), 0644); err != nil {
		return "", fmt.Errorf("failed to locally write metadata file to %s: %w", userdataFilePath, err)
	}

	var command string
	var args []string

	if _, err := exec.LookPath("genisoimage"); err == nil {
		command = "genisoimage"
		args = []string{"-o", isoFilePath, "-volid", "cidata", "-joliet", "-rock", userdataDir}
	} else if _, err := exec.LookPath("mkisofs"); err == nil {
		command = "mkisofs"
		args = []string{"-o", isoFilePath, "-V", "cidata", "-J", "-R", userdataDir}
	} else {
		return "", errors.New("system is missing genisoimage or mkisofs, can't generate userdata iso without it")
	}

	cmd := exec.Command(command, args...)
	if output, err := cmd.CombinedOutput(); err != nil {
		return "", fmt.Errorf("error executing command `%s %s`: output: `%s`, error: `%w`", command, args, string(output), err)
	}

	return isoFilePath, nil
}

func removeFloppyDevice(ctx context.Context, virtualMachine *object.VirtualMachine) error {
	vmDevices, err := virtualMachine.Device(ctx)
	if err != nil {
		return fmt.Errorf("failed to get device list: %w", err)
	}

	// If there is more than one floppy device attached, you will simply get the first one. We
	// assume this won't happen.
	floppyDevice, err := vmDevices.FindFloppy("")
	if err != nil {
		if err.Error() == "no floppy device found" {
			return nil
		}
		return fmt.Errorf("failed to find floppy: %w", err)
	}

	if err := virtualMachine.RemoveDevice(ctx, false, floppyDevice); err != nil {
		return fmt.Errorf("failed to remove floppy device: %w", err)
	}

	return nil
}

func getDisksFromVM(ctx context.Context, vm *object.VirtualMachine) ([]*types.VirtualDisk, error) {
	var props mo.VirtualMachine
	if err := vm.Properties(ctx, vm.Reference(), nil, &props); err != nil {
		return nil, fmt.Errorf("error getting VM template reference: %w", err)
	}
	l := object.VirtualDeviceList(props.Config.Hardware.Device)
	disks := l.SelectByType((*types.VirtualDisk)(nil))

	var result []*types.VirtualDisk
	for _, disk := range disks {
		if assertedDisk := disk.(*types.VirtualDisk); assertedDisk != nil {
			result = append(result, assertedDisk)
		}
	}
	return result, nil
}

func validateDiskResizing(disks []*types.VirtualDisk, requestedSize int64) error {
	if diskLen := len(disks); diskLen != 1 {
		return fmt.Errorf("expected vm to have exactly one disk, got %d", diskLen)
	}
	requestedCapacityInBytes := requestedSize * gigaByte
	if requestedCapacityInBytes < disks[0].CapacityInBytes {
		attachedDiskSizeInGiB := disks[0].CapacityInBytes / gigaByte
		return fmt.Errorf("requested diskSizeGB %d is smaller than size of attached disk(%dGiB)", requestedSize, attachedDiskSizeInGiB)
	}
	return nil
}

// getDatastoreFromVM gets the datastore where the VM files are located.
func getDatastoreFromVM(ctx context.Context, session *Session, vmRef *object.VirtualMachine) (*object.Datastore, error) {
	var props mo.VirtualMachine
	// Obtain VM properties
	if err := vmRef.Properties(ctx, vmRef.Reference(), nil, &props); err != nil {
		return nil, fmt.Errorf("failed to get VM properties: %w", err)
	}
	datastorePathObj := new(object.DatastorePath)
	isSuccess := datastorePathObj.FromString(props.Summary.Config.VmPathName)
	if !isSuccess {
		return nil, fmt.Errorf("failed to parse volPath: %s", props.Summary.Config.VmPathName)
	}
	return session.Finder.Datastore(ctx, datastorePathObj.Datastore)
}

func resolveResourcePoolRef(ctx context.Context, config *Config, session *Session) (*types.ManagedObjectReference, error) {
	if config.ResourcePool != "" {
		targetResourcePool, err := session.Finder.ResourcePool(ctx, config.ResourcePool)
		if err != nil {
			return nil, fmt.Errorf("failed to get target resourcepool: %w", err)
		}
		return types.NewReference(targetResourcePool.Reference()), nil
	}
	return nil, nil
}

func attachTags(ctx context.Context, log *zap.SugaredLogger, config *Config, vm *object.VirtualMachine) error {
	restAPISession, err := NewRESTSession(ctx, config)
	if err != nil {
		return fmt.Errorf("failed to create REST API session: %w", err)
	}
	defer restAPISession.Logout(ctx)
	tagManager := tags.NewManager(restAPISession.Client)
	log.Debug("Attaching tags")
	for _, tag := range config.Tags {
		tagID, err := determineTagID(ctx, tagManager, tag)
		if err != nil {
			return err
		}

		if err := tagManager.AttachTag(ctx, tagID, vm.Reference()); err != nil {
			log.Debugw("Failed to attach tag; it was successfully deleted", "tag", tag)
			return fmt.Errorf("failed to attach tag to VM: %v %w", tag.Name, err)
		}
	}
	return nil
}

func detachTags(ctx context.Context, log *zap.SugaredLogger, config *Config, vm *object.VirtualMachine) error {
	restAPISession, err := NewRESTSession(ctx, config)
	if err != nil {
		return fmt.Errorf("failed to create REST API session: %w", err)
	}
	defer restAPISession.Logout(ctx)
	tagManager := tags.NewManager(restAPISession.Client)

	attachedTags, err := tagManager.GetAttachedTags(ctx, vm.Reference())
	if err != nil {
		return fmt.Errorf("failed to get attached tags for the VM: %s, %w", vm.Name(), err)
	}
	log.Debug("Deleting tags")
	for _, tag := range attachedTags {
		tagID, err := determineTagID(ctx, tagManager, tag)
		if err != nil {
			return err
		}

		err = tagManager.DetachTag(ctx, tagID, vm.Reference())
		if err != nil {
			return fmt.Errorf("failed to delete tag: %v %w", tag, err)
		}
	}

	return nil
}

func determineTagID(ctx context.Context, tagManager *tags.Manager, tag tags.Tag) (string, error) {
	if tag.ID != "" {
		return tag.ID, nil
	}

	apiTag, err := tagManager.GetTagForCategory(ctx, tag.Name, tag.CategoryID)
	if err != nil {
		return "", fmt.Errorf("failed to retrieve tag: %v %w", tag.Name, err)
	}
	return apiTag.ID, nil
}
