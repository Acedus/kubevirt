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

package vmi

import (
	"fmt"
	"slices"
	"strings"

	k8sv1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/equality"
	v1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/sets"

	virtv1 "kubevirt.io/api/core/v1"
	cdiv1 "kubevirt.io/containerized-data-importer-api/pkg/apis/core/v1beta1"

	"kubevirt.io/client-go/log"

	"kubevirt.io/kubevirt/pkg/controller"
	backendstorage "kubevirt.io/kubevirt/pkg/storage/backend-storage"
	storagehotplug "kubevirt.io/kubevirt/pkg/storage/hotplug"
	storagetypes "kubevirt.io/kubevirt/pkg/storage/types"
	"kubevirt.io/kubevirt/pkg/virt-controller/watch/common"
)

// addPVC handles the addition of a PVC, enqueuing affected VMIs.
func (c *Controller) addPVC(obj interface{}) {
	pvc := obj.(*k8sv1.PersistentVolumeClaim)
	if pvc.DeletionTimestamp != nil {
		return
	}
	persistentStateFor, exists := pvc.Labels[backendstorage.PVCPrefix]
	if exists {
		vmiKey := controller.NamespacedKey(pvc.Namespace, persistentStateFor)
		c.pvcExpectations.CreationObserved(vmiKey)
		c.Queue.Add(vmiKey)
		return // The PVC is a backend-storage PVC, won't be listed by `c.listVMIsMatchingDV()`
	}
	vmis, err := c.listVMIsMatchingDV(pvc.Namespace, pvc.Name)
	if err != nil {
		return
	}
	for _, vmi := range vmis {
		log.Log.V(4).Object(pvc).Infof("PVC created for vmi %s", vmi.Name)
		c.enqueueVirtualMachine(vmi)
	}
}

// updatePVC handles updates to a PVC, enqueuing affected VMIs if capacity or requested size changes.
func (c *Controller) updatePVC(old, cur interface{}) {
	curPVC := cur.(*k8sv1.PersistentVolumeClaim)
	oldPVC := old.(*k8sv1.PersistentVolumeClaim)
	if curPVC.ResourceVersion == oldPVC.ResourceVersion {
		// Periodic resync will send update events for all known PVCs.
		// Two different versions of the same PVC will always
		// have different RVs.
		return
	}
	if curPVC.DeletionTimestamp != nil {
		return
	}
	if equality.Semantic.DeepEqual(curPVC.Status.Capacity, oldPVC.Status.Capacity) &&
		equality.Semantic.DeepEqual(curPVC.Spec.Resources.Requests, oldPVC.Spec.Resources.Requests) {
		// We only do something when the capacity or the requested size changes.
		return
	}
	vmis, err := c.listVMIsMatchingDV(curPVC.Namespace, curPVC.Name)
	if err != nil {
		log.Log.Object(curPVC).Errorf("Error encountered getting VMIs for DataVolume: %v", err)
		return
	}
	for _, vmi := range vmis {
		log.Log.V(4).Object(curPVC).Infof("PVC updated for vmi %s", vmi.Name)
		c.enqueueVirtualMachine(vmi)
	}
}

// listVMIsMatchingDV finds all VMIs referencing a given DataVolume or PVC name.
func (c *Controller) listVMIsMatchingDV(namespace, dvName string) ([]*virtv1.VirtualMachineInstance, error) {
	// TODO - refactor if/when dv/pvc do not have the same name
	vmis := []*virtv1.VirtualMachineInstance{}
	for _, indexName := range []string{"dv", "pvc"} {
		objs, err := c.vmiIndexer.ByIndex(indexName, namespace+"/"+dvName)
		if err != nil {
			return nil, err
		}
		for _, obj := range objs {
			vmi := obj.(*virtv1.VirtualMachineInstance)
			vmis = append(vmis, vmi.DeepCopy())
		}
	}
	return vmis, nil
}

// handleBackendStorage manages backend storage PVC creation for the VMI.
func (c *Controller) handleBackendStorage(vmi *virtv1.VirtualMachineInstance) (string, common.SyncError) {
	key, err := controller.KeyFunc(vmi)
	if err != nil {
		return "", common.NewSyncError(err, controller.FailedBackendStorageCreateReason)
	}
	if !backendstorage.IsBackendStorageNeeded(vmi) {
		pvc := backendstorage.PVCForVMI(c.pvcIndexer, vmi)
		if pvc != nil {
			if err = c.backendStorage.DeletePVCForVMI(vmi, pvc.Name); err != nil {
				return "", common.NewSyncError(err, "Failed deleting backend storage")
			}
		}
		return "", nil
	}
	pvc := backendstorage.PVCForVMI(c.pvcIndexer, vmi)
	if pvc == nil {
		c.pvcExpectations.ExpectCreations(key, 1)
		if pvc, err = c.backendStorage.CreatePVCForVMI(vmi); err != nil {
			c.pvcExpectations.CreationObserved(key)
			return "", common.NewSyncError(err, controller.FailedBackendStorageCreateReason)
		}
	}
	return pvc.Name, nil
}

func (c *Controller) processHotplugVolumeStatus(
	vmi *virtv1.VirtualMachineInstance,
	volume storagehotplug.Volume,
	status *virtv1.VolumeStatus,
	attachmentPod *k8sv1.Pod,
) {
	statusCopy := status.DeepCopy()

	if statusCopy.HotplugVolume == nil {
		statusCopy.HotplugVolume = &virtv1.HotplugVolumeStatus{}
	}

	usePVCStatus := false

	if attachmentPod == nil {
		if !c.volumeReady(statusCopy.Phase) {
			statusCopy.HotplugVolume.AttachPodUID = ""
			// Volume is not hotplugged in VM and Pod is gone, or hasn't been created yet, check for the PVC associated with the volume to set phase and message
			usePVCStatus = true
		}
	} else {
		statusCopy.HotplugVolume.AttachPodName = attachmentPod.Name
		if len(attachmentPod.Status.ContainerStatuses) == 1 && attachmentPod.Status.ContainerStatuses[0].Ready {
			statusCopy.HotplugVolume.AttachPodUID = attachmentPod.UID
		} else {
			// Remove UID of old pod if a new one is available, but not yet ready
			statusCopy.HotplugVolume.AttachPodUID = ""
		}
		if canMoveToAttachedPhase(statusCopy.Phase) {
			statusCopy.Phase = virtv1.HotplugVolumeAttachedToNode
			log.Log.V(3).Infof("Setting phase %s for volume %s", statusCopy.Phase, volume.Name)
			statusCopy.Message = fmt.Sprintf("Created hotplug attachment pod %s, for volume %s", attachmentPod.Name, volume.Name)
			statusCopy.Reason = controller.SuccessfulCreatePodReason
			c.recorder.Event(vmi, k8sv1.EventTypeNormal, statusCopy.Reason, statusCopy.Message)
		}
		// Handle race condition where an unplugged volume was quickly re-attached.
		// The old attachment pod may still be cleaning up while the new one is created,
		// causing the new volume to inherit HotplugVolumeDetaching status from the previous
		// hotplug instead of being reset. We reset the phase based on PVC status so it can
		// properly transition through the normal attach flow on the next reconcile.
		if statusCopy.Phase == virtv1.HotplugVolumeDetaching {
			usePVCStatus = true
		}
	}

	if usePVCStatus {
		phase, reason, message := c.getVolumePhaseMessageReason(volume.ClaimName, vmi.Namespace)
		statusCopy.Phase = phase
		log.Log.V(3).Infof("Setting phase %s for volume %s", phase, volume.Name)
		statusCopy.Message = message
		statusCopy.Reason = reason
	}

	*status = *statusCopy
}

func (c *Controller) processPVCInfo(status *virtv1.VolumeStatus, volume storagehotplug.Volume, namespace string) error {
	statusCopy := status.DeepCopy()

	pvcInterface, pvcExists, _ := c.pvcIndexer.GetByKey(controller.NamespacedKey(namespace, volume.ClaimName))
	if pvcExists {
		pvc := pvcInterface.(*k8sv1.PersistentVolumeClaim)
		if volume.Kind == storagehotplug.KindDirectory && storagetypes.IsPVCBlock(pvc.Spec.VolumeMode) {
			statusCopy.Phase = virtv1.VolumePending
			statusCopy.Reason = controller.PVCNotReadyReason
			statusCopy.Message = fmt.Sprintf("Directory volume PVC %s must be filesystem mode, not block mode", volume.ClaimName)
			log.Log.Errorf("Directory volume %s references block mode PVC %s, but directory volumes require filesystem mode", volume.Name, volume.ClaimName)
			*status = *statusCopy
			return nil
		}

		filesystemOverhead, err := c.getFilesystemOverhead(pvc)
		if err != nil {
			log.Log.Reason(err).Errorf("Failed to get filesystem overhead for PVC %s/%s", namespace, volume.ClaimName)
			return err
		}

		statusCopy.PersistentVolumeClaimInfo = &virtv1.PersistentVolumeClaimInfo{
			ClaimName:          pvc.Name,
			AccessModes:        pvc.Spec.AccessModes,
			VolumeMode:         pvc.Spec.VolumeMode,
			Capacity:           pvc.Status.Capacity,
			Requests:           pvc.Spec.Resources.Requests,
			Preallocated:       storagetypes.IsPreallocated(pvc.ObjectMeta.Annotations),
			FilesystemOverhead: &filesystemOverhead,
		}
	}

	*status = *statusCopy
	return nil
}

// updateVolumeStatus updates the VMI's VolumeStatus based on pod and volume state.
func (c *Controller) updateVolumeStatus(vmi *virtv1.VirtualMachineInstance, virtlauncherPod *k8sv1.Pod, dataVolumes []*cdiv1.DataVolume) error {
	hotplugVolumes := storagehotplug.VolumesToAttach(vmi, virtlauncherPod)
	attachmentPods, err := controller.AttachmentPods(virtlauncherPod, c.podIndexer)
	if err != nil {
		return err
	}
	// Attachment pods only hold ready volumes, so a volume that is not ready yet must not keep the
	// others from matching theirs.
	readyHotplugVolumes := c.readyHotplugVolumes(vmi, hotplugVolumes, attachmentPods, dataVolumes)
	attachmentPod, _ := getActiveAndOldAttachmentPods(readyHotplugVolumes, attachmentPods)
	readyHotplugVolumeNames := sets.New[string]()
	for _, volume := range readyHotplugVolumes {
		readyHotplugVolumeNames.Insert(volume.Name)
	}
	attachmentPodFor := func(volumeName string) *k8sv1.Pod {
		if readyHotplugVolumeNames.Has(volumeName) {
			return attachmentPod
		}
		return nil
	}
	hotplugVolumeNames := sets.New[string]()
	for _, volume := range hotplugVolumes {
		hotplugVolumeNames.Insert(volume.Name)
	}

	oldStatuses := make(map[string]virtv1.VolumeStatus, len(vmi.Status.VolumeStatus))
	for _, status := range vmi.Status.DeepCopy().VolumeStatus {
		oldStatuses[status.Name] = status
	}

	newStatuses := make([]virtv1.VolumeStatus, 0)
	if status, exists := c.backendStorageVolumeStatus(vmi, oldStatuses); exists {
		newStatuses = append(newStatuses, status)
	}

	pvcVolumes := storagehotplug.SpecVolumesByName(&vmi.Spec)
	memoryDumpVolumes := memoryDumpVolumeNames(&vmi.Spec)
	specVolumes := specVolumeNames(&vmi.Spec)
	for volumeName := range specVolumes {
		status, exists := oldStatuses[volumeName]
		if !exists {
			status = virtv1.VolumeStatus{Name: volumeName}
		}
		if memoryDumpVolumes.Has(volumeName) && status.MemoryDumpVolume == nil {
			status.MemoryDumpVolume = &virtv1.DomainMemoryDumpInfo{
				ClaimName: volumeName,
			}
		}
		if volume, isPVCVolume := pvcVolumes[volumeName]; isPVCVolume {
			if hotplugVolumeNames.Has(volumeName) {
				c.processHotplugVolumeStatus(vmi, volume, &status, attachmentPodFor(volumeName))
			}
			if err := c.processPVCInfo(&status, volume, vmi.Namespace); err != nil {
				return err
			}
		}
		newStatuses = append(newStatuses, status)
	}

	newStatuses = append(newStatuses, c.detachingVolumeStatuses(vmi, specVolumes, oldStatuses, attachmentPods)...)

	slices.SortStableFunc(newStatuses, func(a, b virtv1.VolumeStatus) int {
		return strings.Compare(a.Name, b.Name)
	})
	vmi.Status.VolumeStatus = newStatuses
	return nil
}

func (c *Controller) backendStorageVolumeStatus(vmi *virtv1.VirtualMachineInstance, oldStatuses map[string]virtv1.VolumeStatus) (virtv1.VolumeStatus, bool) {
	pvc := backendstorage.PVCForVMI(c.pvcIndexer, vmi)
	if pvc == nil {
		return virtv1.VolumeStatus{}, false
	}
	if status, exists := oldStatuses[backendstorage.VolumeName]; exists {
		return status, true
	}
	// TODO https://github.com/kubevirt/kubevirt/issues/17369
	// Fall back to the legacy volume name (the PVC name itself) used by older VMIs
	status, exists := oldStatuses[pvc.Name]
	status.Name = backendstorage.VolumeName
	return status, exists
}

// detachingVolumeStatuses keeps a status removed from the spec while an attachment pod still has it.
func (c *Controller) detachingVolumeStatuses(vmi *virtv1.VirtualMachineInstance, specVolumes sets.Set[string], oldStatuses map[string]virtv1.VolumeStatus, attachmentPods []*k8sv1.Pod) []virtv1.VolumeStatus {
	var statuses []virtv1.VolumeStatus
	for volumeName, status := range oldStatuses {
		if specVolumes.Has(volumeName) {
			continue
		}
		attachmentPod := findAttachmentPodByVolumeName(volumeName, attachmentPods)
		if attachmentPod == nil || status.HotplugVolume == nil {
			log.Log.Object(vmi).V(3).Infof("Deleted status for volume %s", volumeName)
			continue
		}
		status.HotplugVolume.AttachPodName = attachmentPod.Name
		status.HotplugVolume.AttachPodUID = attachmentPod.UID
		status.Phase = phaseForUnpluggedVolume(status.Phase)
		log.Log.V(3).Infof("Setting phase %s for volume %s", status.Phase, volumeName)
		if status.Phase == virtv1.HotplugVolumeDetaching && attachmentPod.DeletionTimestamp != nil {
			status.Message = fmt.Sprintf("Deleted hotplug attachment pod %s, for volume %s", attachmentPod.Name, volumeName)
			status.Reason = controller.SuccessfulDeletePodReason
			c.recorder.Event(vmi, k8sv1.EventTypeNormal, status.Reason, status.Message)
		}
		statuses = append(statuses, status)
	}
	return statuses
}

// specVolumeNames returns the name of every volume in the VMI spec, PVC-backed or not, and of every
// utility volume.
func specVolumeNames(spec *virtv1.VirtualMachineInstanceSpec) sets.Set[string] {
	names := sets.New[string]()
	for i := range spec.Volumes {
		names.Insert(spec.Volumes[i].Name)
	}
	for i := range spec.UtilityVolumes {
		names.Insert(spec.UtilityVolumes[i].Name)
	}
	return names
}

func memoryDumpVolumeNames(spec *virtv1.VirtualMachineInstanceSpec) sets.Set[string] {
	names := sets.New[string]()
	for i := range spec.Volumes {
		if spec.Volumes[i].MemoryDump != nil {
			names.Insert(spec.Volumes[i].Name)
		}
	}
	return names
}

func phaseForUnpluggedVolume(phase virtv1.VolumePhase) virtv1.VolumePhase {
	switch phase {
	case virtv1.VolumeReady:
		return virtv1.VolumeReady
	case virtv1.HotplugVolumeMounted:
		return virtv1.HotplugVolumeMounted
	}
	return virtv1.HotplugVolumeDetaching
}

// volumeReady checks if a volume is in a ready state.
func (c *Controller) volumeReady(phase virtv1.VolumePhase) bool {
	return phase == virtv1.VolumeReady
}

// getVolumePhaseMessageReason determines the phase, reason, and message for a volume.
func (c *Controller) getVolumePhaseMessageReason(claimName string, namespace string) (virtv1.VolumePhase, string, string) {
	pvcInterface, pvcExists, _ := c.pvcIndexer.GetByKey(fmt.Sprintf("%s/%s", namespace, claimName))
	if !pvcExists {
		return virtv1.VolumePending, controller.FailedPvcNotFoundReason, fmt.Sprintf("PVC %s not found", claimName)
	}
	pvc := pvcInterface.(*k8sv1.PersistentVolumeClaim)
	if pvc.Status.Phase == k8sv1.ClaimPending {
		return virtv1.VolumePending, controller.PVCNotReadyReason, "PVC is in phase ClaimPending"
	} else if pvc.Status.Phase == k8sv1.ClaimBound {
		return virtv1.VolumeBound, controller.PVCNotReadyReason, "PVC is in phase Bound"
	}
	return virtv1.VolumePending, controller.PVCNotReadyReason, "PVC is in phase Lost"
}

// getFilesystemOverhead retrieves the filesystem overhead for a PVC.
func (c *Controller) getFilesystemOverhead(pvc *k8sv1.PersistentVolumeClaim) (virtv1.Percent, error) {
	cdiInstances := len(c.cdiStore.List())
	if cdiInstances != 1 {
		if cdiInstances > 1 {
			log.Log.V(3).Object(pvc).Reason(storagetypes.ErrMultipleCdiInstances).Infof(storagetypes.FSOverheadMsg)
		} else {
			log.Log.V(3).Object(pvc).Reason(storagetypes.ErrFailedToFindCdi).Infof(storagetypes.FSOverheadMsg)
		}
		return storagetypes.DefaultFSOverhead, nil
	}
	cdiConfigInterface, cdiConfigExists, err := c.cdiConfigStore.GetByKey(storagetypes.ConfigName)
	if !cdiConfigExists || err != nil {
		return "0", fmt.Errorf("Failed to find CDIConfig but CDI exists: %w", err)
	}
	cdiConfig, ok := cdiConfigInterface.(*cdiv1.CDIConfig)
	if !ok {
		return "0", fmt.Errorf("Failed to convert CDIConfig object %v to type CDIConfig", cdiConfigInterface)
	}
	return storagetypes.GetFilesystemOverhead(pvc.Spec.VolumeMode, pvc.Spec.StorageClassName, cdiConfig)
}

func (c *Controller) syncVolumesUpdate(vmi *virtv1.VirtualMachineInstance) {
	vmiConditions := controller.NewVirtualMachineInstanceConditionManager()
	condition := virtv1.VirtualMachineInstanceCondition{
		Type:               virtv1.VirtualMachineInstanceVolumesChange,
		LastTransitionTime: v1.Now(),
		Status:             k8sv1.ConditionTrue,
		Message:            "migrate volumes",
	}
	vmiConditions.UpdateCondition(vmi, &condition)
}

func (c *Controller) requireVolumesUpdate(vmi *virtv1.VirtualMachineInstance) bool {
	if len(vmi.Status.MigratedVolumes) < 1 {
		return false
	}
	if controller.NewVirtualMachineInstanceConditionManager().HasCondition(vmi, virtv1.VirtualMachineInstanceVolumesChange) {
		return false
	}
	migVolsMap := make(map[string]string)
	for _, v := range vmi.Status.MigratedVolumes {
		migVolsMap[v.SourcePVCInfo.ClaimName] = v.DestinationPVCInfo.ClaimName
	}
	for _, v := range vmi.Spec.Volumes {
		claim := storagetypes.PVCNameFromVirtVolume(&v)
		if claim == "" {
			continue
		}
		if _, ok := migVolsMap[claim]; !ok {
			return true
		}
	}

	return false
}
