package storage

import (
	"fmt"
	"path/filepath"
	"slices"

	v1 "kubevirt.io/api/core/v1"

	cloudinit "kubevirt.io/kubevirt/pkg/cloud-init"
	"kubevirt.io/kubevirt/pkg/config"
	containerdisk "kubevirt.io/kubevirt/pkg/container-disk"
	"kubevirt.io/kubevirt/pkg/emptydisk"
	hostdisk "kubevirt.io/kubevirt/pkg/host-disk"
	"kubevirt.io/kubevirt/pkg/virt-launcher/virtwrap/api"
	"kubevirt.io/kubevirt/pkg/virt-launcher/virtwrap/converter/virtio"
)

const (
	deviceTypeNotCompatibleFmt = "device %s is of type lun. Not compatible with a file based disk"
)

func (d *DomainConfigurator) Convert_v1_Volume_To_api_Disk(vmi *v1.VirtualMachineInstance, source *v1.Volume, disk *api.Disk, diskIndex int) error {

	if source.ContainerDisk != nil {
		return d.Convert_v1_ContainerDiskSource_To_api_Disk(source.Name, source.ContainerDisk, disk, diskIndex)
	}

	if source.CloudInitNoCloud != nil || source.CloudInitConfigDrive != nil {
		return d.Convert_v1_CloudInitSource_To_api_Disk(vmi, source.VolumeSource, disk)
	}

	if source.Sysprep != nil {
		return d.Convert_v1_SysprepSource_To_api_Disk(source.Name, disk)
	}

	if source.HostDisk != nil {
		return d.Convert_v1_HostDisk_To_api_Disk(source.Name, source.HostDisk.Path, disk)
	}

	if source.PersistentVolumeClaim != nil {
		return d.Convert_v1_PersistentVolumeClaim_To_api_Disk(source.Name, disk)
	}

	if source.DataVolume != nil {
		return d.Convert_v1_DataVolume_To_api_Disk(source.Name, disk)
	}

	if source.Ephemeral != nil {
		return d.Convert_v1_EphemeralVolumeSource_To_api_Disk(source.Name, disk)
	}
	if source.EmptyDisk != nil {
		return d.Convert_v1_EmptyDiskSource_To_api_Disk(source.Name, source.EmptyDisk, disk)
	}
	if source.ConfigMap != nil {
		return d.Convert_v1_Config_To_api_Disk(source.Name, disk, config.ConfigMap)
	}
	if source.Secret != nil {
		return d.Convert_v1_Config_To_api_Disk(source.Name, disk, config.Secret)
	}
	if source.DownwardAPI != nil {
		return d.Convert_v1_Config_To_api_Disk(source.Name, disk, config.DownwardAPI)
	}
	if source.ServiceAccount != nil {
		return d.Convert_v1_Config_To_api_Disk(source.Name, disk, config.ServiceAccount)
	}
	if source.DownwardMetrics != nil {
		return d.Convert_v1_DownwardMetricSource_To_api_Disk(disk)
	}

	return fmt.Errorf("disk %s references an unsupported source", disk.Alias.GetName())
}

func (d *DomainConfigurator) Convert_v1_ContainerDiskSource_To_api_Disk(volumeName string, _ *v1.ContainerDiskSource, disk *api.Disk, diskIndex int) error {
	if disk.Type == "lun" {
		return fmt.Errorf(deviceTypeNotCompatibleFmt, disk.Alias.GetName())
	}
	disk.Type = "file"
	setDiskDriver(disk, "qcow2", true)
	disk.Source.File = d.ephemeralDiskCreator.GetFilePath(volumeName)
	disk.BackingStore = &api.BackingStore{
		Format: &api.BackingStoreFormat{},
		Source: &api.DiskSource{},
	}

	source := containerdisk.GetDiskTargetPathFromLauncherView(diskIndex)
	if info := d.disksInfo[volumeName]; info != nil {
		disk.BackingStore.Format.Type = info.Format
	} else {
		return fmt.Errorf("no disk info provided for volume %s", volumeName)
	}
	disk.BackingStore.Source.File = source
	disk.BackingStore.Type = "file"

	return nil
}

func (d *DomainConfigurator) Convert_v1_CloudInitSource_To_api_Disk(vmi *v1.VirtualMachineInstance, source v1.VolumeSource, disk *api.Disk) error {
	if disk.Type == "lun" {
		return fmt.Errorf(deviceTypeNotCompatibleFmt, disk.Alias.GetName())
	}

	var dataSource cloudinit.DataSourceType
	if source.CloudInitNoCloud != nil {
		dataSource = cloudinit.DataSourceNoCloud
	} else if source.CloudInitConfigDrive != nil {
		dataSource = cloudinit.DataSourceConfigDrive
	} else {
		return fmt.Errorf("Only nocloud and configdrive are valid cloud-init volumes")
	}

	disk.Source.File = cloudinit.GetIsoFilePath(dataSource, vmi.Name, vmi.Namespace)
	disk.Type = "file"
	setDiskDriver(disk, "raw", false)
	return nil
}

func (d *DomainConfigurator) Convert_v1_SysprepSource_To_api_Disk(volumeName string, disk *api.Disk) error {
	if disk.Type == "lun" {
		return fmt.Errorf(deviceTypeNotCompatibleFmt, disk.Alias.GetName())
	}

	disk.Source.File = config.GetSysprepDiskPath(volumeName)
	disk.Type = "file"
	disk.Driver.Type = "raw"
	return nil
}

func (d *DomainConfigurator) Convert_v1_HostDisk_To_api_Disk(volumeName string, path string, disk *api.Disk) error {
	disk.Type = "file"
	if cbtPath, ok := d.applyCBT[volumeName]; ok {
		disk.Driver.Type = "qcow2"
		disk.Source.File = cbtPath
		disk.Source.DataStore = &api.DataStore{
			Type: "file",
			Format: &api.DataStoreFormat{
				Type: "raw",
			},
			Source: &api.DiskSource{
				File: hostdisk.GetMountedHostDiskPath(volumeName, path),
			},
		}
	} else {
		disk.Driver.Type = "raw"
		disk.Source.File = hostdisk.GetMountedHostDiskPath(volumeName, path)
	}
	disk.Driver.ErrorPolicy = v1.DiskErrorPolicyStop
	return nil
}

func (d *DomainConfigurator) Convert_v1_PersistentVolumeClaim_To_api_Disk(name string, disk *api.Disk) error {
	return ConvertVolumeSourceToDisk(name, d.applyCBT[name], d.isBlockPVC[name], disk, d.volumesDiscardIgnore)
}

func (d *DomainConfigurator) Convert_v1_DataVolume_To_api_Disk(name string, disk *api.Disk) error {
	return ConvertVolumeSourceToDisk(name, d.applyCBT[name], d.isBlockDV[name], disk, d.volumesDiscardIgnore)
}

func (d *DomainConfigurator) Convert_v1_EphemeralVolumeSource_To_api_Disk(volumeName string, disk *api.Disk) error {
	disk.Type = "file"
	setDiskDriver(disk, "qcow2", true)
	disk.Source.File = d.ephemeralDiskCreator.GetFilePath(volumeName)
	disk.BackingStore = &api.BackingStore{
		Format: &api.BackingStoreFormat{},
		Source: &api.DiskSource{},
	}

	backingDisk := &api.Disk{Driver: &api.DiskDriver{}}
	if d.isBlockPVC[volumeName] {
		if err := Convert_v1_BlockVolumeSource_To_api_Disk(volumeName, backingDisk, d.volumesDiscardIgnore); err != nil {
			return err
		}
	} else {
		if err := Convert_v1_FilesystemVolumeSource_To_api_Disk(volumeName, backingDisk, d.volumesDiscardIgnore); err != nil {
			return err
		}
	}
	disk.BackingStore.Format.Type = backingDisk.Driver.Type
	disk.BackingStore.Source = &backingDisk.Source
	disk.BackingStore.Type = backingDisk.Type

	return nil
}

func (d *DomainConfigurator) Convert_v1_EmptyDiskSource_To_api_Disk(volumeName string, _ *v1.EmptyDiskSource, disk *api.Disk) error {
	if disk.Type == "lun" {
		return fmt.Errorf(deviceTypeNotCompatibleFmt, disk.Alias.GetName())
	}

	disk.Type = "file"
	disk.Source.File = emptydisk.NewEmptyDiskCreator().FilePathForVolumeName(volumeName)
	setDiskDriver(disk, "qcow2", true)

	return nil
}

func (d *DomainConfigurator) Convert_v1_Config_To_api_Disk(volumeName string, disk *api.Disk, configType config.Type) error {
	disk.Type = "file"
	setDiskDriver(disk, "raw", false)
	switch configType {
	case config.ConfigMap:
		disk.Source.File = config.GetConfigMapDiskPath(volumeName)
	case config.Secret:
		disk.Source.File = config.GetSecretDiskPath(volumeName)
	case config.DownwardAPI:
		disk.Source.File = config.GetDownwardAPIDiskPath(volumeName)
	case config.ServiceAccount:
		disk.Source.File = config.GetServiceAccountDiskPath()
	default:
		return fmt.Errorf("Cannot convert config '%s' to disk, unrecognized type", configType)
	}

	return nil
}

func (d *DomainConfigurator) Convert_v1_DownwardMetricSource_To_api_Disk(disk *api.Disk) error {
	disk.Type = "file"
	disk.ReadOnly = toApiReadOnly(true)
	disk.Driver = &api.DiskDriver{
		Type: "raw",
		Name: "qemu",
	}
	// This disk always needs `virtio`. Validation ensures that bus is unset or is already virtio
	disk.Model = virtio.InterpretTransitionalModelType(&d.useVirtioTransitional, d.architecture)
	disk.Source = api.DiskSource{
		File: config.DownwardMetricDisk,
	}
	return nil
}

// Convert_v1_Missing_Volume_To_api_Disk sets defaults when no volume for disk (cdrom, floppy, etc) is provided
func (d *DomainConfigurator) Convert_v1_Missing_Volume_To_api_Disk(disk *api.Disk) error {
	disk.Type = "block"
	disk.Driver.Type = "raw"
	disk.Driver.Discard = "unmap"
	return nil
}

// Convert_v1_Hotplug_Volume_To_api_Disk convers a hotplug volume to an api disk
func (d *DomainConfigurator) Convert_v1_Hotplug_Volume_To_api_Disk(source *v1.Volume, disk *api.Disk) error {
	// This is here because virt-handler before passing the VMI here replaces all PVCs with host disks in
	// hostdisk.ReplacePVCByHostDisk not quite sure why, but it broken hot plugging PVCs
	if source.HostDisk != nil {
		return d.Convert_v1_Hotplug_PersistentVolumeClaim_To_api_Disk(source.Name, disk)
	}

	if source.PersistentVolumeClaim != nil {
		return d.Convert_v1_Hotplug_PersistentVolumeClaim_To_api_Disk(source.Name, disk)
	}

	if source.DataVolume != nil {
		return d.Convert_v1_Hotplug_DataVolume_To_api_Disk(source.Name, disk)
	}
	return fmt.Errorf("hotplug disk %s references an unsupported source", disk.Alias.GetName())
}

// Convert_v1_Hotplug_PersistentVolumeClaim_To_api_Disk converts a Hotplugged PVC to an api disk
func (d *DomainConfigurator) Convert_v1_Hotplug_PersistentVolumeClaim_To_api_Disk(name string, disk *api.Disk) error {
	if d.isBlockPVC[name] {
		return d.Convert_v1_Hotplug_BlockVolumeSource_To_api_Disk(name, disk)
	}
	return d.Convert_v1_Hotplug_FilesystemVolumeSource_To_api_Disk(name, disk)
}

// Convert_v1_Hotplug_DataVolume_To_api_Disk converts a Hotplugged DataVolume to an api disk
func (d *DomainConfigurator) Convert_v1_Hotplug_DataVolume_To_api_Disk(name string, disk *api.Disk) error {
	if d.isBlockDV[name] {
		return d.Convert_v1_Hotplug_BlockVolumeSource_To_api_Disk(name, disk)
	}
	return d.Convert_v1_Hotplug_FilesystemVolumeSource_To_api_Disk(name, disk)
}

// Convert_v1_Hotplug_FilesystemVolumeSource_To_api_Disk takes a FS source and builds the KVM Disk representation
func (d *DomainConfigurator) Convert_v1_Hotplug_FilesystemVolumeSource_To_api_Disk(volumeName string, disk *api.Disk) error {
	disk.Type = "file"
	setDiskDriver(disk, "raw", !slices.Contains(d.volumesDiscardIgnore, volumeName))
	disk.Source.File = GetHotplugFilesystemVolumePath(volumeName)
	return nil
}

// Convert_v1_Hotplug_BlockVolumeSource_To_api_Disk takes a block device source and builds the domain Disk representation
func (d *DomainConfigurator) Convert_v1_Hotplug_BlockVolumeSource_To_api_Disk(volumeName string, disk *api.Disk) error {
	disk.Type = "block"
	setDiskDriver(disk, "raw", !slices.Contains(d.volumesDiscardIgnore, volumeName))
	disk.Source.Dev = GetHotplugBlockDeviceVolumePath(volumeName)
	return nil
}

func ConvertVolumeSourceToDisk(volumeName, cbtPath string, isBlock bool, disk *api.Disk, volumesDiscardIgnore []string) error {
	if cbtPath != "" {
		return convertVolumeWithCBT(volumeName, cbtPath, isBlock, disk, volumesDiscardIgnore)
	}
	return convertVolumeWithoutCBT(volumeName, isBlock, disk, volumesDiscardIgnore)
}

func GetBlockDeviceVolumePath(volumeName string) string {
	return filepath.Join(string(filepath.Separator), "dev", volumeName)
}

func GetFilesystemVolumePath(volumeName string) string {
	return filepath.Join(string(filepath.Separator), "var", "run", "kubevirt-private", "vmi-disks", volumeName, "disk.img")
}

func Convert_v1_BlockVolumeSource_To_api_Disk(volumeName string, disk *api.Disk, volumesDiscardIgnore []string) error {
	disk.Type = "block"
	setDiskDriver(disk, "raw", !slices.Contains(volumesDiscardIgnore, volumeName))
	disk.Source.Name = volumeName
	disk.Source.Dev = GetBlockDeviceVolumePath(volumeName)
	return nil
}

// Convert_v1_FilesystemVolumeSource_To_api_Disk takes a FS source and builds the domain Disk representation
func Convert_v1_FilesystemVolumeSource_To_api_Disk(volumeName string, disk *api.Disk, volumesDiscardIgnore []string) error {
	disk.Type = "file"
	setDiskDriver(disk, "raw", false)
	disk.Source.File = GetFilesystemVolumePath(volumeName)
	if !slices.Contains(volumesDiscardIgnore, volumeName) {
		disk.Driver.Discard = "unmap"
	}
	return nil
}

// GetHotplugFilesystemVolumePath returns the path and file name of a hotplug disk image
func GetHotplugFilesystemVolumePath(volumeName string) string {
	return filepath.Join(string(filepath.Separator), "var", "run", "kubevirt", "hotplug-disks", fmt.Sprintf("%s.img", volumeName))
}

// GetHotplugBlockDeviceVolumePath returns the path and name of a hotplugged block device
func GetHotplugBlockDeviceVolumePath(volumeName string) string {
	return filepath.Join(string(filepath.Separator), "var", "run", "kubevirt", "hotplug-disks", volumeName)
}

func convertVolumeWithCBT(volumeName, cbtPath string, isBlock bool, disk *api.Disk, volumesDiscardIgnore []string) error {
	setDiskDriver(disk, "qcow2", !slices.Contains(volumesDiscardIgnore, volumeName))

	disk.Type = "file"
	disk.Source.File = cbtPath
	disk.Source.DataStore = &api.DataStore{
		Format: &api.DataStoreFormat{
			Type: "raw",
		},
	}

	if isBlock {
		disk.Source.Name = volumeName
		disk.Source.DataStore.Type = "block"
		disk.Source.DataStore.Source = &api.DiskSource{
			Dev: GetBlockDeviceVolumePath(volumeName),
		}
	} else {
		disk.Source.DataStore.Type = "file"
		disk.Source.DataStore.Source = &api.DiskSource{
			File: GetFilesystemVolumePath(volumeName),
		}
	}

	return nil
}

func convertVolumeWithoutCBT(volumeName string, isBlock bool, disk *api.Disk, volumesDiscardIgnore []string) error {
	setDiskDriver(disk, "raw", !slices.Contains(volumesDiscardIgnore, volumeName))

	if isBlock {
		disk.Type = "block"
		disk.Source.Name = volumeName
		disk.Source.Dev = GetBlockDeviceVolumePath(volumeName)
	} else {
		disk.Type = "file"
		disk.Source.File = GetFilesystemVolumePath(volumeName)
	}
	return nil
}

func setDiskDriver(disk *api.Disk, driverType string, discard bool) {
	disk.Driver.Type = driverType
	disk.Driver.ErrorPolicy = v1.DiskErrorPolicyStop
	if discard {
		disk.Driver.Discard = "unmap"
	}
}
