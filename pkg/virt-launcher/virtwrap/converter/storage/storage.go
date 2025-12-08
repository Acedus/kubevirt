/*
 * This file is part of the KubeVirt project
 *
 * Licensed under the Apache License, Version 2.0 (the "License");
 * you may not use this file except in compliance with the License.
 * You may obtain a copy of the License at
 *
 *     http://www.apache.org/licenses/LICENSE-2.0
 *
 * Unless required by applicable law or agreed to in writing, software
 * distributed under the License is distributed on an "AS IS" BASIS,
 * WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
 * See the License for the specific language governing permissions and
 * limitations under the License.
 *
 * Copyright The KubeVirt Authors.
 *
 */

package storage

import (
	"fmt"

	v1 "kubevirt.io/api/core/v1"

	ephemeraldisk "kubevirt.io/kubevirt/pkg/ephemeral-disk"
	"kubevirt.io/kubevirt/pkg/os/disk"
	"kubevirt.io/kubevirt/pkg/virt-launcher/virtwrap/api"
)

type DomainConfigurator struct {
	architecture          string
	hotplugVolumes        map[string]v1.VolumeStatus
	permanentVolumes      map[string]v1.VolumeStatus
	disksInfo             map[string]*disk.DiskInfo
	isBlockPVC            map[string]bool
	isBlockDV             map[string]bool
	applyCBT              map[string]string
	useVirtioTransitional bool
	useLaunchSecuritySEV  bool
	useLaunchSecurityPV   bool
	expandDisksEnabled    bool
	useBlkMQ              bool
	vcpus                 uint
	volumesDiscardIgnore  []string
	ephemeralDiskCreator  ephemeraldisk.EphemeralDiskCreatorInterface
}

type option func(*DomainConfigurator)

func NewDomainConfigurator(options ...option) DomainConfigurator {
	var configurator DomainConfigurator

	for _, f := range options {
		f(&configurator)
	}

	return configurator
}

func (d DomainConfigurator) Configure(vmi *v1.VirtualMachineInstance, domain *api.Domain) error {
	volumeIndices := map[string]int{}
	volumes := map[string]*v1.Volume{}
	for i, volume := range vmi.Spec.Volumes {
		volumes[volume.Name] = volume.DeepCopy()
		volumeIndices[volume.Name] = i
	}

	volumeStatusMap := make(map[string]v1.VolumeStatus)
	for _, volumeStatus := range vmi.Status.VolumeStatus {
		volumeStatusMap[volumeStatus.Name] = volumeStatus
	}

	prefixMap := newDeviceNamer(vmi.Status.VolumeStatus, vmi.Spec.Domain.Devices.Disks)
	for _, disk := range vmi.Spec.Domain.Devices.Disks {
		newDisk := api.Disk{}
		emptyCDRom := false

		if err := d.Convert_v1_Disk_To_api_Disk(&disk, &newDisk, prefixMap, volumeStatusMap); err != nil {
			return err
		}
		volume := volumes[disk.Name]
		if volume == nil {
			if disk.CDRom == nil {
				return fmt.Errorf("no matching volume with name %s found", disk.Name)
			}
			emptyCDRom = true
		}

		hpStatus, hpOk := d.hotplugVolumes[disk.Name]
		var err error
		switch {
		case emptyCDRom:
			err = d.Convert_v1_Missing_Volume_To_api_Disk(&newDisk)
		case hpOk:
			err = d.Convert_v1_Hotplug_Volume_To_api_Disk(volume, &newDisk)
		default:
			err = d.Convert_v1_Volume_To_api_Disk(vmi, volume, &newDisk, volumeIndices[disk.Name])
		}

		if err != nil {
			return err
		}

		if err := Convert_v1_BlockSize_To_api_BlockIO(&disk, &newDisk); err != nil {
			return err
		}

		_, isPermVolume := d.permanentVolumes[disk.Name]
		// if len(c.PermanentVolumes) == 0, it means the vmi is not ready yet, add all disks
		permReady := isPermVolume || len(d.permanentVolumes) == 0
		hotplugReady := hpOk && (hpStatus.Phase == v1.HotplugVolumeMounted || hpStatus.Phase == v1.VolumeReady)

		if permReady || hotplugReady || emptyCDRom {
			domain.Spec.Devices.Disks = append(domain.Spec.Devices.Disks, newDisk)
		}
		if err := setErrorPolicy(&disk, &newDisk); err != nil {
			return err
		}
	}
	// Handle virtioFS
	domain.Spec.Devices.Filesystems = append(domain.Spec.Devices.Filesystems, convertFileSystems(vmi.Spec.Domain.Devices.Filesystems)...)

	domain.Spec.Devices.PanicDevices = append(domain.Spec.Devices.PanicDevices, convertPanicDevices(vmi.Spec.Domain.Devices.PanicDevices)...)

	d.setIOThreads(vmi, domain, d.vcpus)

	return nil
}

func (d *DomainConfigurator) numBlkQueues() *uint {
	if !d.useBlkMQ {
		return nil
	}
	return &d.vcpus
}

func WithArchitecture(architecture string) option {
	return func(d *DomainConfigurator) {
		d.architecture = architecture
	}
}

func WithUseLaunchSecuritySEV(useLaunchSecuritySEV bool) option {
	return func(d *DomainConfigurator) {
		d.useLaunchSecuritySEV = useLaunchSecuritySEV
	}
}

func WithUseLaunchSecurityPV(useLaunchSecurityPV bool) option {
	return func(d *DomainConfigurator) {
		d.useLaunchSecurityPV = useLaunchSecurityPV
	}
}

func WithHotplugVolumes(hotplugVolumes map[string]v1.VolumeStatus) option {
	return func(d *DomainConfigurator) {
		d.hotplugVolumes = hotplugVolumes
	}
}

func WithPermanentVolumes(permanentVolumes map[string]v1.VolumeStatus) option {
	return func(d *DomainConfigurator) {
		d.permanentVolumes = permanentVolumes
	}
}

func WithUseVirtioTransitional(useVirtioTranslation bool) option {
	return func(d *DomainConfigurator) {
		d.useVirtioTransitional = useVirtioTranslation
	}
}

func WithExpandDisksEnabled(expandDisksEnabled bool) option {
	return func(d *DomainConfigurator) {
		d.expandDisksEnabled = expandDisksEnabled
	}
}

func WithVolumesDiscardIgnore(volumesDiscardIgnore []string) option {
	return func(d *DomainConfigurator) {
		d.volumesDiscardIgnore = volumesDiscardIgnore
	}
}

func WithEphemeralDiskCreator(ephemeralDiskCreator ephemeraldisk.EphemeralDiskCreatorInterface) option {
	return func(d *DomainConfigurator) {
		d.ephemeralDiskCreator = ephemeralDiskCreator
	}
}

func WithDisksInfo(disksInfo map[string]*disk.DiskInfo) option {
	return func(d *DomainConfigurator) {
		d.disksInfo = disksInfo
	}
}

func WithApplyCBT(applyCBT map[string]string) option {
	return func(d *DomainConfigurator) {
		d.applyCBT = applyCBT
	}
}

func WithIsBlockPVC(isBlockPVC map[string]bool) option {
	return func(d *DomainConfigurator) {
		d.isBlockPVC = isBlockPVC
	}
}

func WithIsBlockDV(isBlockDV map[string]bool) option {
	return func(d *DomainConfigurator) {
		d.isBlockDV = isBlockDV
	}
}

func WithUseBlkMQ(useBlkMQ bool) option {
	return func(d *DomainConfigurator) {
		d.useBlkMQ = useBlkMQ
	}
}

func WithVcpus(count uint) option {
	return func(d *DomainConfigurator) {
		d.vcpus = count
		if d.vcpus == 0 {
			d.vcpus = 1
		}
	}
}

func setErrorPolicy(diskDevice *v1.Disk, apiDisk *api.Disk) error {
	if diskDevice.ErrorPolicy == nil {
		apiDisk.Driver.ErrorPolicy = v1.DiskErrorPolicyStop
		return nil
	}
	switch *diskDevice.ErrorPolicy {
	case v1.DiskErrorPolicyStop, v1.DiskErrorPolicyIgnore, v1.DiskErrorPolicyReport, v1.DiskErrorPolicyEnospace:
		apiDisk.Driver.ErrorPolicy = *diskDevice.ErrorPolicy
	default:
		return fmt.Errorf("error policy %s not recognized", *diskDevice.ErrorPolicy)
	}
	return nil
}

func convertPanicDevices(panicDevices []v1.PanicDevice) []api.PanicDevice {
	var domainPanicDevices []api.PanicDevice

	for _, panicDevice := range panicDevices {
		domainPanicDevices = append(domainPanicDevices, api.PanicDevice{Model: panicDevice.Model})
	}

	return domainPanicDevices
}

func hasIOThreads(vmi *v1.VirtualMachineInstance) bool {
	if vmi.Spec.Domain.IOThreadsPolicy != nil {
		return true
	}
	for _, diskDevice := range vmi.Spec.Domain.Devices.Disks {
		if diskDevice.DedicatedIOThread != nil && *diskDevice.DedicatedIOThread {
			return true
		}
	}
	return false
}
