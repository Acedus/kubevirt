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

package types

import (
	"k8s.io/apimachinery/pkg/api/resource"

	v1 "kubevirt.io/api/core/v1"
)

func IsStorageVolume(volume *v1.Volume) bool {
	return volume.PersistentVolumeClaim != nil || volume.DataVolume != nil
}

func IsDeclarativeHotplugVolume(vol *v1.Volume) bool {
	if vol == nil {
		return false
	}
	volSrc := vol.VolumeSource
	if volSrc.PersistentVolumeClaim != nil && volSrc.PersistentVolumeClaim.Hotpluggable {
		return true
	}
	if volSrc.DataVolume != nil && volSrc.DataVolume.Hotpluggable {
		return true
	}

	return false
}

func IsHotpluggableVolumeSource(vol *v1.Volume) bool {
	return IsStorageVolume(vol) ||
		vol.MemoryDump != nil
}

func IsHotplugVolume(vol *v1.Volume) bool {
	if vol == nil {
		return false
	}
	if IsDeclarativeHotplugVolume(vol) {
		return true
	}
	volSrc := vol.VolumeSource
	if volSrc.MemoryDump != nil && volSrc.MemoryDump.PersistentVolumeClaimVolumeSource.Hotpluggable {
		return true
	}

	return false
}

func GetVolumesByName(vmiSpec *v1.VirtualMachineInstanceSpec) map[string]*v1.Volume {
	volumes := map[string]*v1.Volume{}
	for _, vol := range vmiSpec.Volumes {
		volumes[vol.Name] = vol.DeepCopy()
	}
	return volumes
}

func GetFilesystemsFromVolumes(vmi *v1.VirtualMachineInstance) map[string]*v1.Filesystem {
	fs := map[string]*v1.Filesystem{}

	for _, f := range vmi.Spec.Domain.Devices.Filesystems {
		fs[f.Name] = f.DeepCopy()
	}

	return fs
}

func IsMigratedVolume(name string, vmi *v1.VirtualMachineInstance) bool {
	for _, v := range vmi.Status.MigratedVolumes {
		if v.VolumeName == name {
			return true
		}
	}
	return false
}

func GetTotalSizeMigratedVolumes(vmi *v1.VirtualMachineInstance) *resource.Quantity {
	size := int64(0)
	for _, v := range vmi.Status.MigratedVolumes {
		if v.SourcePVCInfo == nil {
			continue
		}
		if s := GetDiskCapacity(v.SourcePVCInfo); s != nil {
			size += *s
		}
	}

	return resource.NewQuantity(size, resource.BinarySI)
}
