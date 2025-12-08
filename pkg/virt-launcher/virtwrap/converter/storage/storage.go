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
	v1 "kubevirt.io/api/core/v1"

	"kubevirt.io/kubevirt/pkg/virt-launcher/virtwrap/api"
)

type DomainConfigurator struct {
	diskConfigurator DiskConfiguratorInterface
	vcpus            uint
	useBlkMQ         bool
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

		if err := d.diskConfigurator.Configure(&disk, &newDisk, prefixMap, d.numBlkQueues(), volumeStatusMap); err != nil {
			return err
		}
	}

	return nil
}

func WithDiskConfigurator(diskConfigurator DiskConfiguratorInterface) option {
	return func(d *DomainConfigurator) {
		d.diskConfigurator = diskConfigurator
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

func (d *DomainConfigurator) numBlkQueues() *uint {
	if !d.useBlkMQ {
		return nil
	}
	return &d.vcpus
}

func toApiReadOnly(src bool) *api.ReadOnly {
	if src {
		return &api.ReadOnly{}
	}
	return nil
}
