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

package cbt

import (
	"context"
	"fmt"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	k8stypes "k8s.io/apimachinery/pkg/types"

	backupv1 "kubevirt.io/api/backup/v1alpha1"
	v1 "kubevirt.io/api/core/v1"
	"kubevirt.io/client-go/log"

	"kubevirt.io/kubevirt/pkg/apimachinery/patch"
	"kubevirt.io/kubevirt/pkg/pointer"
)

const (
	pullScratchPVC        = "backup-scratch-pvc"
	MinScratchSize        = "2Gi"
	ScratchOverheadFactor = 0.10
)

var (
	failedScratchPVCAttach      = "failed to attach backup scratch pvc: %s"
	failedScratchPVCDetach      = "failed to detach backup scratch pvc: %s"
	failedScratchPVCSizeCalc    = "failed to calculate scratch size: %w"
	failedScratchPVCCreate      = "failed to create scratch pvc: %w"
	attachScratchPVCMsg         = "attaching backup scratch pvc %s to vmi %s"
	detachScratchPVCMsg         = "detaching backup scratch pvc from vmi %s"
	pvcExistsWithDifferentOwner = "PVC %s already exists but is not owned by backup %s"
)

func (ctrl *VMBackupController) getOrCreateBackupScratchPVC(backup *backupv1.VirtualMachineBackup, vmi *v1.VirtualMachineInstance, pvcName string) *SyncInfo {
	objKey := cacheKeyFunc(backup.Namespace, pvcName)
	obj, exists, err := ctrl.pvcStore.GetByKey(objKey)
	if err != nil {
		err = fmt.Errorf("error getting PVC from store: %w", err)
		log.Log.Error(err.Error())
		return syncInfoError(err)
	}

	if exists {
		pvc := obj.(*corev1.PersistentVolumeClaim)
		if !metav1.IsControlledBy(pvc, backup) {
			// This is a collision: A PVC with this name exists but belongs to someone else.
			// We likely need to error out rather than hijack it.
			return syncInfoError(fmt.Errorf(pvcExistsWithDifferentOwner, pvcName, backup.Name))
		}
		return nil
	}

	return ctrl.createBackupScratchPVC(backup, pvcName, vmi)
}

func (ctrl *VMBackupController) createBackupScratchPVC(backup *backupv1.VirtualMachineBackup, pvcName string, vmi *v1.VirtualMachineInstance) *SyncInfo {
	scratchSize, err := ctrl.calculateScratchSize(backup, vmi)
	if err != nil {
		return syncInfoError(fmt.Errorf(failedScratchPVCSizeCalc, err))
	}

	fsMode := corev1.PersistentVolumeFilesystem

	scratchPvc := &corev1.PersistentVolumeClaim{
		ObjectMeta: metav1.ObjectMeta{
			Name:      pvcName,
			Namespace: backup.Namespace,
			OwnerReferences: []metav1.OwnerReference{
				*metav1.NewControllerRef(backup, backupv1.SchemeGroupVersion.WithKind("VirtualMachineBackup")),
			},
		},
		Spec: corev1.PersistentVolumeClaimSpec{
			AccessModes: []corev1.PersistentVolumeAccessMode{
				corev1.ReadWriteOnce,
			},
			VolumeMode: &fsMode,
			Resources: corev1.VolumeResourceRequirements{
				Requests: corev1.ResourceList{
					corev1.ResourceStorage: *scratchSize,
				},
			},
		},
	}

	_, err = ctrl.client.CoreV1().PersistentVolumeClaims(backup.Namespace).Create(context.Background(), scratchPvc, metav1.CreateOptions{})
	if err != nil {
		return syncInfoError(fmt.Errorf(failedScratchPVCCreate, err))
	}

	return nil
}

func (ctrl *VMBackupController) calculateScratchSize(backup *backupv1.VirtualMachineBackup, vmi *v1.VirtualMachineInstance) (*resource.Quantity, error) {
	vmi, exists, err := ctrl.getVMI(backup)
	if err != nil {
		err = fmt.Errorf("failed to get VMI from store: %w", err)
		log.Log.Error(err.Error())
		return nil, err
	}
	if !exists {
		return nil, err
	}

	var totalSize int64 = 0
	for _, volume := range vmi.Spec.Volumes {
		if IsCBTEligibleVolume(&volume) {
			totalSize += getCapacityFromVMIStatus(vmi, &volume)
		}
	}

	scratchBytes := float64(totalSize) * ScratchOverheadFactor

	minQuantity := resource.MustParse(MinScratchSize)
	if int64(scratchBytes) < minQuantity.Value() {
		return &minQuantity, nil
	}

	finalQuantity := resource.NewQuantity(int64(scratchBytes), resource.BinarySI)
	return finalQuantity, nil
}

func getCapacityFromVMIStatus(vmi *v1.VirtualMachineInstance, volume *v1.Volume) int64 {
	for _, volumeStatus := range vmi.Status.VolumeStatus {
		if volumeStatus.Name != volume.Name {
			continue
		}

		if volume.PersistentVolumeClaim != nil || volume.DataVolume != nil {
			if volumeStatus.PersistentVolumeClaimInfo != nil {
				return quantFromPvcInfo(volumeStatus.PersistentVolumeClaimInfo)
			}
		}

		if volume.HostDisk != nil {
			// Check for converted host-disk
			if volumeStatus.PersistentVolumeClaimInfo != nil {
				return quantFromPvcInfo(volumeStatus.PersistentVolumeClaimInfo)
			}
			return volume.HostDisk.Capacity.Value()
		}
	}
	return 0
}

func quantFromPvcInfo(pvcInfo *v1.PersistentVolumeClaimInfo) int64 {
	if quant, ok := pvcInfo.Capacity[corev1.ResourceStorage]; ok {
		return quant.Value()
	}
	return 0
}

func (ctrl *VMBackupController) backupScratchPVCAttached(vmi *v1.VirtualMachineInstance) bool {
	if vmi == nil {
		return false
	}
	for _, volumeStatus := range vmi.Status.VolumeStatus {
		if volumeStatus.Name == pullScratchPVC {
			return volumeStatus.HotplugVolume != nil && volumeStatus.Phase == v1.HotplugVolumeMounted
		}
	}
	return false
}

func (ctrl *VMBackupController) backupScratchPVCDetached(vmi *v1.VirtualMachineInstance) bool {
	if vmi == nil {
		return true
	}
	for _, volumeStatus := range vmi.Status.VolumeStatus {
		if volumeStatus.Name == pullScratchPVC {
			return false
		}
	}
	return true
}

func (ctrl *VMBackupController) detachBackupScratchPVC(vmi *v1.VirtualMachineInstance) *SyncInfo {
	if len(vmi.Spec.UtilityVolumes) == 0 {
		return nil
	}

	newUtilityVolumes := make([]v1.UtilityVolume, 0, len(vmi.Spec.UtilityVolumes))
	for _, vol := range vmi.Spec.UtilityVolumes {
		if vol.Name != pullScratchPVC {
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
		failedPatchErr := fmt.Errorf(failedScratchPVCDetach, err)
		log.Log.Object(vmi).Errorf("Failed to generate patch: %s", failedPatchErr.Error())
		return syncInfoError(failedPatchErr)
	}

	_, err = ctrl.client.VirtualMachineInstance(vmi.Namespace).Patch(context.Background(), vmi.Name, k8stypes.JSONPatchType, patchBytes, metav1.PatchOptions{})
	if err != nil {
		failedPatchErr := fmt.Errorf(failedTargetPVCDetach, err)
		log.Log.Object(vmi).Errorf("Failed to patch VMI: %s", failedPatchErr.Error())
		return syncInfoError(failedPatchErr)
	}

	pvcDetachMsg := fmt.Sprintf(detachTargetPVCMsg, vmi.Name)
	log.Log.Object(vmi).Infof("%s", pvcDetachMsg)

	return &SyncInfo{
		event:  backupTargetPVCRemoveVolumeSubmitted,
		reason: pvcDetachMsg,
	}
}

func (ctrl *VMBackupController) attachBackupScratchPVC(vmi *v1.VirtualMachineInstance, pvcName string) *SyncInfo {
	// Check if we already patched the VMI with the utilityVolume
	for _, vol := range vmi.Spec.UtilityVolumes {
		if vol.Name == pullScratchPVC {
			return nil
		}
	}

	backupVolume := v1.UtilityVolume{
		Name: pullScratchPVC,
		PersistentVolumeClaimVolumeSource: corev1.PersistentVolumeClaimVolumeSource{
			ClaimName: pvcName,
		},
		Type: pointer.P(v1.Backup),
	}

	patchSet := patch.New(
		patch.WithTest("/spec/utilityVolumes", vmi.Spec.UtilityVolumes),
	)

	newUtilityVolumes := append(vmi.Spec.UtilityVolumes, backupVolume)
	if len(vmi.Spec.UtilityVolumes) > 0 {
		patchSet.AddOption(patch.WithReplace("/spec/utilityVolumes", newUtilityVolumes))
	} else {
		patchSet.AddOption(patch.WithAdd("/spec/utilityVolumes", newUtilityVolumes))
	}

	patchBytes, err := patchSet.GeneratePayload()
	if err != nil {
		err = fmt.Errorf("failed to generate attach backup target PVC patch: %w", err)
		log.Log.Error(err.Error())
		return syncInfoError(err)
	}

	_, err = ctrl.client.VirtualMachineInstance(vmi.Namespace).Patch(context.Background(), vmi.Name, k8stypes.JSONPatchType, patchBytes, metav1.PatchOptions{})
	if err != nil {
		failedPatchErr := fmt.Errorf(failedTargetPVCAttach, err)
		log.Log.Object(vmi).Errorf("%s", failedPatchErr.Error())
		return syncInfoError(failedPatchErr)
	}

	pvcAttachMsg := fmt.Sprintf(attachScratchPVCMsg, pvcName, vmi.Name)
	log.Log.Object(vmi).Infof("%s", pvcAttachMsg)

	return &SyncInfo{
		event:  backupTargetPVCAddVolumeSubmitted,
		reason: pvcAttachMsg,
	}
}
