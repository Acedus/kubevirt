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

package memorydump

import (
	"context"
	"fmt"

	k8score "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/cache"

	v1 "kubevirt.io/api/core/v1"
	"kubevirt.io/client-go/kubecli"
	"kubevirt.io/client-go/log"

	"kubevirt.io/kubevirt/pkg/apimachinery/patch"
	"kubevirt.io/kubevirt/pkg/pointer"
	storagetypes "kubevirt.io/kubevirt/pkg/storage/types"
)

const (
	ErrorReason = "MemoryDumpError"
	failed      = "Memory dump failed"
)

func HasCompleted(vm *v1.VirtualMachine) bool {
	return vm.Status.MemoryDumpRequest != nil && vm.Status.MemoryDumpRequest.Phase != v1.MemoryDumpAssociating && vm.Status.MemoryDumpRequest.Phase != v1.MemoryDumpInProgress
}

// CleanVMTemplateMemoryDumpVolumes strips deprecated MemoryDump volumes from the VM template.
// These volumes use the deprecated VolumeSource.MemoryDump field and should be converted
// to UtilityVolumes. Only removes volumes that have the deprecated MemoryDump source set
// and whose name matches the memory dump request's claim name.
func CleanVMTemplateMemoryDumpVolumes(vm *v1.VirtualMachine) {
	if vm.Status.MemoryDumpRequest == nil {
		return
	}
	claimName := vm.Status.MemoryDumpRequest.ClaimName
	newVolumes := make([]v1.Volume, 0, len(vm.Spec.Template.Spec.Volumes))
	for _, volume := range vm.Spec.Template.Spec.Volumes {
		if volume.Name == claimName && volume.MemoryDump != nil {
			continue
		}
		newVolumes = append(newVolumes, volume)
	}
	vm.Spec.Template.Spec.Volumes = newVolumes
}

func HandleRequest(client kubecli.KubevirtClient, vm *v1.VirtualMachine, vmi *v1.VirtualMachineInstance, pvcStore cache.Store) error {
	if vm.Status.MemoryDumpRequest == nil {
		return nil
	}

	vmiUtilityVolumeMap := make(map[string]v1.UtilityVolume)
	if vmi != nil {
		for _, vol := range vmi.Spec.UtilityVolumes {
			vmiUtilityVolumeMap[vol.Name] = vol
		}
	}

	switch vm.Status.MemoryDumpRequest.Phase {
	case v1.MemoryDumpAssociating:
		if vmi == nil || vmi.DeletionTimestamp != nil || !vmi.IsRunning() {
			return nil
		}
		if _, exists := vmiUtilityVolumeMap[vm.Status.MemoryDumpRequest.ClaimName]; exists {
			return nil
		}
		if err := attachMemoryDumpVolume(client, vmi, vm.Status.MemoryDumpRequest.ClaimName); err != nil {
			log.Log.Object(vmi).Errorf("unable to attach memory dump utility volume: %v", err)
			return err
		}
	case v1.MemoryDumpUnmounting, v1.MemoryDumpFailed:
		if err := patchMemoryDumpPVCAnnotation(client, vm, pvcStore); err != nil {
			return err
		}
		if _, exists := vmiUtilityVolumeMap[vm.Status.MemoryDumpRequest.ClaimName]; !exists {
			return nil
		}
		if err := detachMemoryDumpVolume(client, vmi, vm.Status.MemoryDumpRequest.ClaimName); err != nil {
			log.Log.Object(vmi).Errorf("unable to detach memory dump utility volume: %v", err)
			return err
		}
	case v1.MemoryDumpDissociating:
		if _, exists := vmiUtilityVolumeMap[vm.Status.MemoryDumpRequest.ClaimName]; exists {
			if err := detachMemoryDumpVolume(client, vmi, vm.Status.MemoryDumpRequest.ClaimName); err != nil {
				log.Log.Object(vmi).Errorf("unable to detach memory dump utility volume: %v", err)
				return err
			}
		}
	}

	return nil
}

func UpdateRequest(vm *v1.VirtualMachine, vmi *v1.VirtualMachineInstance) {
	if vm.Status.MemoryDumpRequest == nil {
		return
	}

	updatedMemoryDumpReq := vm.Status.MemoryDumpRequest.DeepCopy()

	if vm.Status.MemoryDumpRequest.Remove {
		updatedMemoryDumpReq.Phase = v1.MemoryDumpDissociating
	}

	switch vm.Status.MemoryDumpRequest.Phase {
	case v1.MemoryDumpCompleted:
		return
	case v1.MemoryDumpAssociating:
		if vmi != nil {
			for _, vol := range vmi.Spec.UtilityVolumes {
				if vm.Status.MemoryDumpRequest.ClaimName == vol.Name {
					updatedMemoryDumpReq.Phase = v1.MemoryDumpInProgress
					break
				}
			}
		}
	case v1.MemoryDumpInProgress:
		if vmi != nil && len(vmi.Status.VolumeStatus) > 0 {
			for _, volumeStatus := range vmi.Status.VolumeStatus {
				if volumeStatus.Name == vm.Status.MemoryDumpRequest.ClaimName &&
					volumeStatus.MemoryDumpVolume != nil {
					if volumeStatus.MemoryDumpVolume.StartTimestamp != nil {
						updatedMemoryDumpReq.StartTimestamp = volumeStatus.MemoryDumpVolume.StartTimestamp
					}
					if volumeStatus.Phase == v1.MemoryDumpVolumeCompleted {
						updatedMemoryDumpReq.Phase = v1.MemoryDumpUnmounting
						updatedMemoryDumpReq.EndTimestamp = volumeStatus.MemoryDumpVolume.EndTimestamp
						updatedMemoryDumpReq.FileName = &volumeStatus.MemoryDumpVolume.TargetFileName
					} else if volumeStatus.Phase == v1.MemoryDumpVolumeFailed {
						updatedMemoryDumpReq.Phase = v1.MemoryDumpFailed
						updatedMemoryDumpReq.Message = volumeStatus.Message
						updatedMemoryDumpReq.EndTimestamp = volumeStatus.MemoryDumpVolume.EndTimestamp
					}
				}
			}
		}
	case v1.MemoryDumpUnmounting:
		if vmi != nil {
			for _, volumeStatus := range vmi.Status.VolumeStatus {
				if volumeStatus.Name == vm.Status.MemoryDumpRequest.ClaimName {
					return
				}
			}
		}
		updatedMemoryDumpReq.Phase = v1.MemoryDumpCompleted
	case v1.MemoryDumpDissociating:
		if vmi != nil {
			for _, volumeStatus := range vmi.Status.VolumeStatus {
				if volumeStatus.Name == vm.Status.MemoryDumpRequest.ClaimName {
					return
				}
			}
		}
		updatedMemoryDumpReq = nil
	}

	vm.Status.MemoryDumpRequest = updatedMemoryDumpReq
}

func attachMemoryDumpVolume(client kubecli.KubevirtClient, vmi *v1.VirtualMachineInstance, claimName string) error {
	for _, vol := range vmi.Spec.UtilityVolumes {
		if vol.Name == claimName {
			return nil
		}
	}

	memoryDumpVolume := v1.UtilityVolume{
		Name: claimName,
		PersistentVolumeClaimVolumeSource: k8score.PersistentVolumeClaimVolumeSource{
			ClaimName: claimName,
		},
		Type: pointer.P(v1.MemoryDump),
	}

	patchSet := patch.New(
		patch.WithTest("/spec/utilityVolumes", vmi.Spec.UtilityVolumes),
	)

	newUtilityVolumes := append(vmi.Spec.UtilityVolumes, memoryDumpVolume)
	if len(vmi.Spec.UtilityVolumes) > 0 {
		patchSet.AddOption(patch.WithReplace("/spec/utilityVolumes", newUtilityVolumes))
	} else {
		patchSet.AddOption(patch.WithAdd("/spec/utilityVolumes", newUtilityVolumes))
	}

	patchBytes, err := patchSet.GeneratePayload()
	if err != nil {
		return fmt.Errorf("failed to generate attach memory dump volume patch: %w", err)
	}

	_, err = client.VirtualMachineInstance(vmi.Namespace).Patch(context.Background(), vmi.Name, types.JSONPatchType, patchBytes, metav1.PatchOptions{})
	return err
}

func detachMemoryDumpVolume(client kubecli.KubevirtClient, vmi *v1.VirtualMachineInstance, claimName string) error {
	if len(vmi.Spec.UtilityVolumes) == 0 {
		return nil
	}

	newUtilityVolumes := make([]v1.UtilityVolume, 0, len(vmi.Spec.UtilityVolumes))
	for _, vol := range vmi.Spec.UtilityVolumes {
		if vol.Name != claimName {
			newUtilityVolumes = append(newUtilityVolumes, vol)
		}
	}

	patchSet := patch.New(
		patch.WithTest("/spec/utilityVolumes", vmi.Spec.UtilityVolumes),
	)
	if len(newUtilityVolumes) == 0 {
		patchSet.AddOption(patch.WithRemove("/spec/utilityVolumes"))
	} else {
		patchSet.AddOption(patch.WithReplace("/spec/utilityVolumes", newUtilityVolumes))
	}

	patchBytes, err := patchSet.GeneratePayload()
	if err != nil {
		return fmt.Errorf("failed to generate detach memory dump volume patch: %w", err)
	}

	_, err = client.VirtualMachineInstance(vmi.Namespace).Patch(context.Background(), vmi.Name, types.JSONPatchType, patchBytes, metav1.PatchOptions{})
	return err
}

func patchMemoryDumpPVCAnnotation(client kubecli.KubevirtClient, vm *v1.VirtualMachine, pvcStore cache.Store) error {
	request := vm.Status.MemoryDumpRequest
	pvc, err := storagetypes.GetPersistentVolumeClaimFromCache(vm.Namespace, request.ClaimName, pvcStore)
	if err != nil {
		log.Log.Object(vm).Errorf("Error getting PersistentVolumeClaim to update memory dump annotation: %v", err)
		return err
	}
	if pvc == nil {
		log.Log.Object(vm).Errorf("Error getting PersistentVolumeClaim to update memory dump annotation: %v", err)
		return fmt.Errorf("Error when trying to update memory dump annotation, pvc %s not found", request.ClaimName)
	}

	var patchVal string
	switch request.Phase {
	case v1.MemoryDumpUnmounting:
		// skip patching pvc annotation if file name
		// is empty
		if request.FileName == nil {
			return nil
		}
		patchVal = *request.FileName
	case v1.MemoryDumpFailed:
		patchVal = failed
	default:
		log.Log.Object(vm).Errorf("Unexpected phase when patching memory dump pvc annotation")
		return nil
	}

	annoPatch := patch.New()
	if len(pvc.Annotations) == 0 {
		annoPatch.AddOption(patch.WithAdd("/metadata/annotations", map[string]string{v1.PVCMemoryDumpAnnotation: patchVal}))
	} else if ann, ok := pvc.Annotations[v1.PVCMemoryDumpAnnotation]; ok && ann == patchVal {
		return nil
	} else {
		annoPatch.AddOption(patch.WithReplace("/metadata/annotations/"+patch.EscapeJSONPointer(v1.PVCMemoryDumpAnnotation), patchVal))
	}

	annoPatchPayload, err := annoPatch.GeneratePayload()
	if err != nil {
		return err
	}

	_, err = client.CoreV1().PersistentVolumeClaims(pvc.Namespace).Patch(context.Background(), pvc.Name, types.JSONPatchType, annoPatchPayload, metav1.PatchOptions{})
	if err != nil {
		log.Log.Object(vm).Errorf("failed to annotate memory dump PVC %s/%s, error: %s", pvc.Namespace, pvc.Name, err)
	}

	return nil
}
