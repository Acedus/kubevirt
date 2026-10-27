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
	"encoding/json"
	"fmt"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/equality"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/cache"

	backupv1 "kubevirt.io/api/backup/v1alpha1"
	v1 "kubevirt.io/api/core/v1"
	"kubevirt.io/client-go/log"

	"kubevirt.io/kubevirt/pkg/controller"
	storagetypes "kubevirt.io/kubevirt/pkg/storage/types"
)

const (
	vmHeldForOfflineBackupMsg   = "vm %s is held for an offline backup"
	launcherPodsRunningMsg      = "vm %s still has virt-launcher pods, waiting for them to terminate"
	offlineNoVolumesMsg         = "vm %s has no volumes eligible for an offline backup"
	offlineExporterFailedMsg    = "offline backup exporter failed: %s"
	offlineDeletedMsg           = "backup was deleted before it was exported"
	offlineCheckpointTimeFormat = "2006-01-02_15-04-05"

	OfflineBackupPrepareContainerName = "backup-prepare"

	virtualMachineExportKind = "VirtualMachineExport"
)

func (ctrl *VMBackupController) handleUpdateVM(oldObj, newObj interface{}) {
	oldVM, ok := oldObj.(*v1.VirtualMachine)
	if !ok {
		return
	}
	newVM, ok := newObj.(*v1.VirtualMachine)
	if !ok {
		return
	}
	if oldVM.Generation == newVM.Generation && equality.Semantic.DeepEqual(oldVM.Status, newVM.Status) {
		return
	}
	ctrl.enqueueBackupsOfVM(newVM)
}

func (ctrl *VMBackupController) handleDeleteVM(obj interface{}) {
	if unknown, ok := obj.(cache.DeletedFinalStateUnknown); ok && unknown.Obj != nil {
		obj = unknown.Obj
	}
	vm, ok := obj.(*v1.VirtualMachine)
	if !ok {
		return
	}
	ctrl.enqueueBackupsOfVM(vm)
}

func (ctrl *VMBackupController) enqueueBackupsOfVM(vm *v1.VirtualMachine) {
	key := types.NamespacedName{Namespace: vm.Namespace, Name: vm.Name}.String()
	for _, backupKey := range backupKeysOfVM(ctrl.backupInformer.GetIndexer(), ctrl.backupTrackerInformer.GetIndexer(), key) {
		ctrl.backupQueue.Add(backupKey)
	}
}

func (ctrl *VMBackupController) holdSourceVM(backup *backupv1.VirtualMachineBackup, sourceName string) (bool, error) {
	vm, err := ctrl.getVM(backup.Namespace, sourceName)
	if err != nil || vm == nil {
		return false, err
	}
	if holder := vm.Status.BackupInProgress; holder != nil {
		return *holder == backup.Name, nil
	}
	if vm.DeletionTimestamp != nil || vm.Status.Created || controller.IsSetToStart(vm, nil) ||
		vm.Status.SnapshotInProgress != nil || vm.Status.RestoreInProgress != nil {
		return false, nil
	}

	vmCopy := vm.DeepCopy()
	vmCopy.Status.BackupInProgress = new(backup.Name)
	if _, err := ctrl.client.VirtualMachine(vm.Namespace).UpdateStatus(context.Background(), vmCopy, metav1.UpdateOptions{}); err != nil {
		return false, fmt.Errorf("failed to hold VM %s for backup %s: %w", vm.Name, backup.Name, err)
	}
	log.Log.Object(backup).Infof("Holding VM %s for an offline backup", vm.Name)
	return true, nil
}

func (ctrl *VMBackupController) releaseSourceVM(backup *backupv1.VirtualMachineBackup) error {
	sourceName, err := BackupSourceVMName(ctrl.backupTrackerInformer.GetStore(), backup)
	if err != nil || sourceName == "" {
		return err
	}
	vm, err := ctrl.getVM(backup.Namespace, sourceName)
	if err != nil || vm == nil {
		return err
	}
	if holder := vm.Status.BackupInProgress; holder == nil || *holder != backup.Name {
		return nil
	}

	vmCopy := vm.DeepCopy()
	vmCopy.Status.BackupInProgress = nil
	if _, err := ctrl.client.VirtualMachine(vm.Namespace).UpdateStatus(context.Background(), vmCopy, metav1.UpdateOptions{}); err != nil {
		return fmt.Errorf("failed to release VM %s from backup %s: %w", vm.Name, backup.Name, err)
	}
	log.Log.Object(backup).Infof("Released VM %s from its offline backup", vm.Name)
	return nil
}

func (ctrl *VMBackupController) getVM(namespace, name string) (*v1.VirtualMachine, error) {
	obj, exists, err := ctrl.vmStore.GetByKey(types.NamespacedName{Namespace: namespace, Name: name}.String())
	if err != nil || !exists {
		return nil, err
	}
	return obj.(*v1.VirtualMachine), nil
}

func (ctrl *VMBackupController) heldSourceVM(backup *backupv1.VirtualMachineBackup, sourceName string) (*v1.VirtualMachine, bool, error) {
	vm, err := ctrl.getVM(backup.Namespace, sourceName)
	if err != nil || vm == nil {
		return nil, false, err
	}
	holder := vm.Status.BackupInProgress
	return vm, holder != nil && *holder == backup.Name, nil
}

func (ctrl *VMBackupController) reconcileOffline(backup *backupv1.VirtualMachineBackup, vm *v1.VirtualMachine, backupTracker *backupv1.VirtualMachineBackupTracker, backupDeleting bool) error {
	if backupDeleting {
		return ctrl.finishDeletedOfflineBackup(backup, backupTracker)
	}
	if backup.Status.StartTimestamp == nil {
		return ctrl.startOfflineBackup(backup, vm, backupTracker)
	}
	if isPullMode(backup) && isPullBackupTTLExpired(backup) {
		if done, err := ctrl.stopOfflineExport(backup); err != nil || !done {
			return err
		}
		ctrl.setFailed(backup, backupv1.ReasonFailed, backupTTLExpiredMsg)
		return nil
	}

	vmExport, err := ctrl.getOrCreateBackupExport(backup)
	if err != nil || vmExport == nil {
		return err
	}
	pod, err := ctrl.exporterPod(backup)
	if err != nil || pod == nil {
		return err
	}
	if err := applyPrepareReport(backup, pod); err != nil {
		return err
	}

	switch pod.Status.Phase {
	case corev1.PodFailed:
		if err := ctrl.cleanupBackupExport(backup); err != nil {
			return err
		}
		ctrl.setFailed(backup, backupv1.ReasonFailed, fmt.Sprintf(offlineExporterFailedMsg, podFailureMessage(pod)))
		return nil
	case corev1.PodSucceeded:
		if backup.IsPushMode() {
			return ctrl.completeOfflineBackup(backup, backupTracker)
		}
	}
	if isPullMode(backup) {
		return ctrl.populateExportLinks(backup, vmExport)
	}
	return nil
}

func (ctrl *VMBackupController) startOfflineBackup(backup *backupv1.VirtualMachineBackup, vm *v1.VirtualMachine, backupTracker *backupv1.VirtualMachineBackupTracker) error {
	if running, err := ctrl.hasLauncherPods(vm); err != nil {
		return err
	} else if running {
		setInitializing(backup, fmt.Sprintf(launcherPodsRunningMsg, vm.Name))
		return nil
	}
	if cbtState := vm.Status.ChangedBlockTracking; cbtState == nil || cbtState.State != v1.ChangedBlockTrackingEnabled {
		ctrl.setFailed(backup, backupv1.ReasonFailed, fmt.Sprintf(vmNoChangedBlockTrackingMsg, vm.Name))
		return nil
	}
	volumes := offlineBackupVolumes(vm)
	if len(volumes) == 0 {
		ctrl.setFailed(backup, backupv1.ReasonFailed, fmt.Sprintf(offlineNoVolumesMsg, vm.Name))
		return nil
	}
	if backup.IsPushMode() {
		if reason, err := ctrl.verifyBackupTargetPVC(backup.Spec.PvcName, backup.Namespace); err != nil || reason != "" {
			if reason != "" {
				setInitializing(backup, reason)
			}
			return err
		}
	}

	if err := ctrl.addBackupFinalizer(backup); err != nil {
		return err
	}
	if backupTracker != nil {
		if err := ctrl.addTrackerFinalizer(backupTracker); err != nil {
			return err
		}
	}

	backupType := backupv1.Full
	if IsIncrementalBackup(backup, backupTracker) {
		backupType = backupv1.Incremental
	}
	for i := range volumes {
		volumes[i].Type = backupType
	}
	backup.Status.IncludedVolumes = volumes
	backup.Status.CheckpointName = new(fmt.Sprintf("%s-%s", backup.Name, backup.CreationTimestamp.UTC().Format(offlineCheckpointTimeFormat)))
	backup.Status.StartTimestamp = new(metav1.Now())
	setProgressing(backup)
	log.Log.Object(backup).Infof("Started offline %s backup of VM %s", backupType, vm.Name)
	return nil
}

func (ctrl *VMBackupController) completeOfflineBackup(backup *backupv1.VirtualMachineBackup, backupTracker *backupv1.VirtualMachineBackupTracker) error {
	if err := ctrl.advanceTrackerToOfflineCheckpoint(backup, backupTracker); err != nil {
		return err
	}
	if err := ctrl.cleanupBackupExport(backup); err != nil {
		return err
	}
	ctrl.setQuiescedCondition(backup, string(backupv1.QuiesceSkipped))
	setComplete(backup)
	ctrl.recorder.Eventf(backup, corev1.EventTypeNormal, backupCompletedEvent, backupCompleted)
	return nil
}

// A pull backup ends when its consumer deletes it, which completes it once it was exported.
func (ctrl *VMBackupController) finishDeletedOfflineBackup(backup *backupv1.VirtualMachineBackup, backupTracker *backupv1.VirtualMachineBackupTracker) error {
	if done, err := ctrl.stopOfflineExport(backup); err != nil || !done {
		return err
	}
	if !isPullMode(backup) || !isBackupExportReady(backup) {
		ctrl.setFailed(backup, backupv1.ReasonFailed, offlineDeletedMsg)
		return nil
	}
	return ctrl.completeOfflineBackup(backup, backupTracker)
}

func isBackupExportReady(backup *backupv1.VirtualMachineBackup) bool {
	cond := meta.FindStatusCondition(backupConditions(backup), string(backupv1.ConditionProgressing))
	return cond != nil && cond.Reason == backupv1.ReasonExportReady
}

func (ctrl *VMBackupController) advanceTrackerToOfflineCheckpoint(backup *backupv1.VirtualMachineBackup, backupTracker *backupv1.VirtualMachineBackupTracker) error {
	if backupTracker == nil || backup.Status.CheckpointName == nil {
		return nil
	}
	checkpoint := backupv1.BackupCheckpoint{Name: *backup.Status.CheckpointName, CreationTime: backup.Status.StartTimestamp}
	return ctrl.updateBackupTracker(backup.Namespace, backupTracker, checkpoint)
}

// The VM stays held until the exporter pod, which has its disks open, is gone.
func (ctrl *VMBackupController) stopOfflineExport(backup *backupv1.VirtualMachineBackup) (bool, error) {
	if err := ctrl.cleanupBackupExport(backup); err != nil {
		return false, err
	}
	pod, err := ctrl.exporterPod(backup)
	if err != nil {
		return false, err
	}
	if pod != nil && !controller.PodIsDown(pod) {
		return false, fmt.Errorf("waiting for the exporter pod of backup %s to terminate: %w", backup.Name, errCleanupPending)
	}
	return true, nil
}

func (ctrl *VMBackupController) exporterPod(backup *backupv1.VirtualMachineBackup) (*corev1.Pod, error) {
	objs, err := ctrl.podIndexer.ByIndex(cache.NamespaceIndex, backup.Namespace)
	if err != nil {
		return nil, err
	}
	for _, obj := range objs {
		pod := obj.(*corev1.Pod)
		if owner := metav1.GetControllerOf(pod); owner != nil && owner.Kind == virtualMachineExportKind && owner.Name == backup.Name {
			return pod, nil
		}
	}
	return nil, nil
}

func (ctrl *VMBackupController) hasLauncherPods(vm *v1.VirtualMachine) (bool, error) {
	objs, err := ctrl.podIndexer.ByIndex(cache.NamespaceIndex, vm.Namespace)
	if err != nil {
		return false, err
	}
	for _, obj := range objs {
		pod := obj.(*corev1.Pod)
		if pod.Labels[v1.AppLabel] == "virt-launcher" && pod.Annotations[v1.DomainAnnotation] == vm.Name && !controller.PodIsDown(pod) {
			return true, nil
		}
	}
	return false, nil
}

func (ctrl *VMBackupController) handlePod(obj interface{}) {
	if unknown, ok := obj.(cache.DeletedFinalStateUnknown); ok && unknown.Obj != nil {
		obj = unknown.Obj
	}
	pod, ok := obj.(*corev1.Pod)
	if !ok {
		return
	}
	if owner := metav1.GetControllerOf(pod); owner != nil && owner.Kind == virtualMachineExportKind {
		key := types.NamespacedName{Namespace: pod.Namespace, Name: owner.Name}.String()
		if _, exists, _ := ctrl.backupInformer.GetStore().GetByKey(key); exists {
			ctrl.backupQueue.Add(key)
		}
		return
	}
	if pod.Labels[v1.AppLabel] == "virt-launcher" {
		vmKey := types.NamespacedName{Namespace: pod.Namespace, Name: pod.Annotations[v1.DomainAnnotation]}.String()
		for _, key := range backupKeysOfVM(ctrl.backupInformer.GetIndexer(), ctrl.backupTrackerInformer.GetIndexer(), vmKey) {
			ctrl.backupQueue.Add(key)
		}
	}
}

// applyPrepareReport takes the volumes virt-backup-prepare had to fall back to a full backup.
func applyPrepareReport(backup *backupv1.VirtualMachineBackup, pod *corev1.Pod) error {
	for _, status := range pod.Status.InitContainerStatuses {
		if status.Name != OfflineBackupPrepareContainerName || status.State.Terminated == nil || status.State.Terminated.ExitCode != 0 {
			continue
		}
		var fallbacks []backupv1.BackupVolumeInfo
		if err := json.Unmarshal([]byte(status.State.Terminated.Message), &fallbacks); err != nil {
			return fmt.Errorf("failed to parse the offline backup prepare report: %w", err)
		}
		for _, fallback := range fallbacks {
			for i := range backup.Status.IncludedVolumes {
				if backup.Status.IncludedVolumes[i].VolumeName == fallback.VolumeName {
					backup.Status.IncludedVolumes[i].Type = fallback.Type
				}
			}
		}
	}
	return nil
}

func podFailureMessage(pod *corev1.Pod) string {
	for _, status := range append(pod.Status.InitContainerStatuses, pod.Status.ContainerStatuses...) {
		if terminated := status.State.Terminated; terminated != nil && terminated.ExitCode != 0 {
			if terminated.Message != "" {
				return terminated.Message
			}
			return fmt.Sprintf("container %s exited with %d", status.Name, terminated.ExitCode)
		}
	}
	return pod.Status.Message
}

func offlineBackupVolumes(vm *v1.VirtualMachine) []backupv1.BackupVolumeInfo {
	if vm.Spec.Template == nil {
		return nil
	}
	var volumes []backupv1.BackupVolumeInfo
	for i := range vm.Spec.Template.Spec.Volumes {
		volume := &vm.Spec.Template.Spec.Volumes[i]
		if IsCBTEligibleVolume(volume) && storagetypes.PVCNameFromVirtVolume(volume) != "" {
			volumes = append(volumes, backupv1.BackupVolumeInfo{VolumeName: volume.Name})
		}
	}
	return volumes
}
