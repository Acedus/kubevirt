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

package hotplug

import (
	"fmt"
	"slices"

	k8sv1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/util/sets"
	"k8s.io/client-go/tools/cache"

	v1 "kubevirt.io/api/core/v1"

	storagetypes "kubevirt.io/kubevirt/pkg/storage/types"
)

// Kind describes how a PVC-backed volume is exposed to virt-launcher.
type Kind string

const (
	// KindDisk is exposed as a disk image or block device and attached to the domain.
	KindDisk Kind = "Disk"
	// KindDirectory is bind-mounted as a directory into virt-launcher and never becomes a domain disk.
	KindDirectory Kind = "Directory"
)

// Volume describes how a PVC-backed volume of a VMI is attached, independent of why it exists.
type Volume struct {
	Name      string
	ClaimName string
	Kind      Kind
}

// SpecVolumes returns every PVC-backed volume in the VMI spec, hot- or cold-plugged,
// with spec.volumes entries first followed by spec.utilityVolumes, each in spec order.
func SpecVolumes(spec *v1.VirtualMachineInstanceSpec) []Volume {
	volumes := make([]Volume, 0, len(spec.Volumes)+len(spec.UtilityVolumes))
	for i := range spec.Volumes {
		volume := &spec.Volumes[i]
		if !storagetypes.IsHotpluggableVolumeSource(volume) {
			continue
		}
		kind := KindDisk
		if volume.MemoryDump != nil {
			kind = KindDirectory
		}
		volumes = append(volumes, Volume{Name: volume.Name, ClaimName: storagetypes.PVCNameFromVirtVolume(volume), Kind: kind})
	}
	for _, utilityVolume := range spec.UtilityVolumes {
		volumes = append(volumes, Volume{Name: utilityVolume.Name, ClaimName: utilityVolume.ClaimName, Kind: KindDirectory})
	}
	return volumes
}

// SpecVolumesByName indexes SpecVolumes by volume name. The webhook keeps names unique across
// spec.volumes and spec.utilityVolumes, so no entry overwrites another.
func SpecVolumesByName(spec *v1.VirtualMachineInstanceSpec) map[string]Volume {
	volumes := SpecVolumes(spec)
	volumesByName := make(map[string]Volume, len(volumes))
	for _, volume := range volumes {
		volumesByName[volume.Name] = volume
	}
	return volumesByName
}

// VolumesToAttach returns the SpecVolumes an attachment pod has to serve: those the
// virt-launcher pod does not already have.
func VolumesToAttach(vmi *v1.VirtualMachineInstance, launcherPod *k8sv1.Pod) []Volume {
	podVolumes := sets.New[string]()
	for _, podVolume := range launcherPod.Spec.Volumes {
		podVolumes.Insert(podVolume.Name)
	}
	return slices.DeleteFunc(SpecVolumes(&vmi.Spec), func(volume Volume) bool {
		return podVolumes.Has(volume.Name)
	})
}

// PVCsByVolumeName looks up the claim of every volume in the PVC store, keyed by volume name.
func PVCsByVolumeName(volumes []Volume, pvcStore cache.Store, namespace string) (map[string]*k8sv1.PersistentVolumeClaim, error) {
	pvcs := make(map[string]*k8sv1.PersistentVolumeClaim, len(volumes))
	for _, volume := range volumes {
		pvc, err := storagetypes.GetPersistentVolumeClaimFromCache(namespace, volume.ClaimName, pvcStore)
		if err != nil {
			return nil, err
		}
		if pvc == nil {
			return nil, fmt.Errorf("claim %s not found", volume.ClaimName)
		}
		pvcs[volume.Name] = pvc
	}
	return pvcs, nil
}
