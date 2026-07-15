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
	"errors"
	"fmt"
	"slices"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/equality"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	backupv1 "kubevirt.io/api/backup/v1alpha1"
	v1 "kubevirt.io/api/core/v1"
	"kubevirt.io/client-go/log"

	"kubevirt.io/kubevirt/pkg/controller"
	migrations "kubevirt.io/kubevirt/pkg/util/migrations"
)

const (
	DefaultForceDeleteTimeout  = 24 * time.Hour
	ForceDeleteAfterAnnotation = "backup.kubevirt.io/force-delete-after"
)

func isTrackerDeleting(tracker *backupv1.VirtualMachineBackupTracker) bool {
	return tracker != nil && tracker.DeletionTimestamp != nil
}

func trackerNeedsCheckpointRedefinition(tracker *backupv1.VirtualMachineBackupTracker) bool {
	return tracker != nil &&
		tracker.Status != nil &&
		tracker.Status.CheckpointRedefinitionRequired != nil &&
		*tracker.Status.CheckpointRedefinitionRequired &&
		len(tracker.Status.Checkpoints) > 0
}

func trackerNeedsPruning(tracker *backupv1.VirtualMachineBackupTracker) bool {
	if tracker == nil || tracker.Status == nil {
		return false
	}
	retain := tracker.Spec.RetainCheckpoints
	return retain != nil && int32(len(tracker.Status.Checkpoints)) > *retain
}

func (ctrl *VMBackupController) runTrackerWorker() {
	for ctrl.ExecuteTracker() {
	}
}

func (ctrl *VMBackupController) ExecuteTracker() bool {
	key, quit := ctrl.trackerQueue.Get()
	if quit {
		return false
	}
	defer ctrl.trackerQueue.Done(key)

	err := ctrl.executeTracker(key)
	if err != nil {
		if errors.Is(err, errActiveBackups) {
			log.Log.V(4).Infof("reenqueuing VirtualMachineBackupTracker %v: %v", key, err)
		} else {
			log.Log.Reason(err).Infof("reenqueuing VirtualMachineBackupTracker %v", key)
		}
		ctrl.trackerQueue.AddRateLimited(key)
	} else {
		log.Log.V(4).Infof("processed VirtualMachineBackupTracker %v", key)
		ctrl.trackerQueue.Forget(key)
	}
	return true
}

func (ctrl *VMBackupController) executeTracker(key string) error {
	logger := log.Log.With("VirtualMachineBackupTracker", key)
	logger.V(3).Infof("Processing tracker %s", key)

	storeObj, exists, err := ctrl.backupTrackerInformer.GetStore().GetByKey(key)
	if err != nil {
		logger.Errorf("Error getting tracker from store: %v", err)
		return err
	}
	if !exists {
		logger.V(3).Infof("Tracker %s no longer exists in store", key)
		return nil
	}

	tracker, ok := storeObj.(*backupv1.VirtualMachineBackupTracker)
	if !ok {
		logger.Errorf("Unexpected resource type: %T", storeObj)
		return fmt.Errorf("unexpected resource %+v", storeObj)
	}

	trackerCopy := tracker.DeepCopy()
	syncErr := ctrl.syncTracker(trackerCopy)
	if syncErr != nil {
		logger.V(3).Infof("Reconciling VirtualMachineBackupTracker %s failed: %v", key, syncErr)
	}

	if !equality.Semantic.DeepEqual(tracker.Status, trackerCopy.Status) {
		if _, err := ctrl.client.VirtualMachineBackupTracker(trackerCopy.Namespace).UpdateStatus(
			context.Background(), trackerCopy, metav1.UpdateOptions{}); err != nil {
			logger.Reason(err).Errorf("Updating VirtualMachineBackupTracker %s status failed", key)
			return err
		}
	}

	return syncErr
}

func (ctrl *VMBackupController) syncTracker(tracker *backupv1.VirtualMachineBackupTracker) error {
	if isTrackerDeleting(tracker) && controller.HasFinalizer(tracker, backupv1.VirtualMachineBackupTrackerFinalizer) {
		return ctrl.handleTrackerDeletion(tracker)
	}

	needsRedefinition := trackerNeedsCheckpointRedefinition(tracker)
	needsPruning := trackerNeedsPruning(tracker)
	if !needsRedefinition && !needsPruning {
		return nil
	}

	vmiName := tracker.Spec.Source.Name
	vmi, exists, err := ctrl.getVMI(tracker.Namespace, vmiName)
	if err != nil {
		return fmt.Errorf("failed to get VMI %s/%s: %w", tracker.Namespace, vmiName, err)
	}
	if !exists || vmi == nil {
		return fmt.Errorf("VMI %s/%s not found, deferring tracker operations", tracker.Namespace, vmiName)
	}
	if migrations.IsMigrating(vmi) {
		return fmt.Errorf("VMI %s/%s is migrating, deferring tracker operations", tracker.Namespace, vmiName)
	}

	if needsRedefinition {
		if err := ctrl.syncBackupTracker(tracker); err != nil {
			return err
		}
	}

	if needsPruning {
		if err := ctrl.pruneExcessCheckpoints(tracker); err != nil {
			return err
		}
	}

	return nil
}

func (ctrl *VMBackupController) syncBackupTracker(tracker *backupv1.VirtualMachineBackupTracker) error {
	logger := log.Log.With("VirtualMachineBackupTracker", tracker.Name)

	vmiName := tracker.Spec.Source.Name
	vmiClient := ctrl.client.VirtualMachineInstance(tracker.Namespace)
	checkpoints := tracker.Status.Checkpoints

	for i := range checkpoints {
		parentName := ""
		if i > 0 {
			parentName = checkpoints[i-1].Name
		}

		logger.Infof("Redefining checkpoint %s (parent=%s) for VMI %s", checkpoints[i].Name, parentName, vmiName)
		err := vmiClient.RedefineCheckpoint(context.Background(), vmiName, &checkpoints[i], parentName)
		if err == nil {
			continue
		}

		if !isCheckpointInvalidError(err) {
			return err
		}

		truncated := checkpoints[i:]
		tracker.Status.Checkpoints = checkpoints[:i]

		for j := len(truncated) - 1; j >= 0; j-- {
			logger.Infof("Deleting orphaned checkpoint %s from VMI %s", truncated[j].Name, vmiName)
			_ = vmiClient.DeleteCheckpoint(context.Background(), vmiName, truncated[j].Name)
		}

		ctrl.recorder.Eventf(tracker, corev1.EventTypeWarning, "CheckpointRedefinitionFailed",
			"Checkpoint %s invalid, truncated chain to %d checkpoints. Next backup will be full.",
			checkpoints[i].Name, i)
		break
	}

	if len(tracker.Status.Checkpoints) > 0 {
		tracker.Status.LatestCheckpoint = &tracker.Status.Checkpoints[len(tracker.Status.Checkpoints)-1]
	} else {
		tracker.Status.LatestCheckpoint = nil
	}
	tracker.Status.CheckpointRedefinitionRequired = nil

	return nil
}

func isCheckpointInvalidError(err error) bool {
	return apierrors.IsInvalid(err)
}

func forceDeleteTimeout(tracker *backupv1.VirtualMachineBackupTracker) time.Duration {
	if v, ok := tracker.Annotations[ForceDeleteAfterAnnotation]; ok {
		d, err := time.ParseDuration(v)
		if err != nil {
			log.Log.Warningf("Invalid %s annotation %q on tracker %s/%s, using default %v",
				ForceDeleteAfterAnnotation, v, tracker.Namespace, tracker.Name, DefaultForceDeleteTimeout)
			return DefaultForceDeleteTimeout
		}
		return d
	}
	return DefaultForceDeleteTimeout
}

func (ctrl *VMBackupController) pruneExcessCheckpoints(tracker *backupv1.VirtualMachineBackupTracker) error {
	retain := tracker.Spec.RetainCheckpoints
	if retain == nil {
		return nil
	}

	excess := int32(len(tracker.Status.Checkpoints)) - *retain
	if excess <= 0 {
		return nil
	}

	hasActive, err := ctrl.trackerHasActiveBackups(tracker)
	if err != nil {
		return fmt.Errorf("failed to check active backups: %w", err)
	}
	if hasActive {
		return fmt.Errorf("tracker %s/%s has active backups, deferring checkpoint pruning", tracker.Namespace, tracker.Name)
	}

	vmiName := tracker.Spec.Source.Name

	for excess > 0 {
		cp := tracker.Status.Checkpoints[0]
		log.Log.Infof("Pruning checkpoint %s from tracker %s/%s via DeleteCheckpoint RPC",
			cp.Name, tracker.Namespace, tracker.Name)
		if err := ctrl.client.VirtualMachineInstance(tracker.Namespace).DeleteCheckpoint(
			context.Background(), vmiName, cp.Name); err != nil {
			return fmt.Errorf("failed to delete checkpoint %s: %w", cp.Name, err)
		}
		tracker.Status.Checkpoints = tracker.Status.Checkpoints[1:]
		excess--
	}

	if len(tracker.Status.Checkpoints) > 0 {
		tracker.Status.LatestCheckpoint = &tracker.Status.Checkpoints[len(tracker.Status.Checkpoints)-1]
	} else {
		tracker.Status.LatestCheckpoint = nil
	}
	return nil
}

func (ctrl *VMBackupController) handleTrackerDeletion(tracker *backupv1.VirtualMachineBackupTracker) error {
	logger := log.Log.With("VirtualMachineBackupTracker", tracker.Name)
	logger.Infof("Handling deletion for tracker %s/%s", tracker.Namespace, tracker.Name)

	hasActiveBackups, err := ctrl.trackerHasActiveBackups(tracker)
	if err != nil {
		return fmt.Errorf("failed to check active backups for tracker %s/%s: %w", tracker.Namespace, tracker.Name, err)
	}
	if hasActiveBackups {
		return fmt.Errorf("tracker %s/%s has active backups, cannot remove finalizer: %w", tracker.Namespace, tracker.Name, errActiveBackups)
	}

	if tracker.Status == nil || len(tracker.Status.Checkpoints) == 0 {
		return ctrl.removeTrackerFinalizer(tracker)
	}

	vmiName := tracker.Spec.Source.Name
	vmi, exists, err := ctrl.getVMI(tracker.Namespace, vmiName)
	if err != nil {
		return fmt.Errorf("failed to get VMI %s/%s: %w", tracker.Namespace, vmiName, err)
	}

	if !exists {
		vmKey := fmt.Sprintf("%s/%s", tracker.Namespace, vmiName)
		_, vmExists, vmErr := ctrl.vmStore.GetByKey(vmKey)
		if vmErr != nil {
			return fmt.Errorf("failed to check VM %s: %w", vmKey, vmErr)
		}
		if !vmExists {
			logger.Infof("VM %s/%s permanently deleted, clearing checkpoints and removing finalizer", tracker.Namespace, vmiName)
			tracker.Status.Checkpoints = nil
			tracker.Status.LatestCheckpoint = nil
			return ctrl.removeTrackerFinalizer(tracker)
		}
		if time.Since(tracker.DeletionTimestamp.Time) > forceDeleteTimeout(tracker) {
			logger.Warningf("Tracker %s/%s deletion timed out waiting for VM %s to start — clearing checkpoints (bitmaps may be orphaned)",
				tracker.Namespace, tracker.Name, vmiName)
			ctrl.recorder.Eventf(tracker, corev1.EventTypeWarning, "ForceDeleteTimeout",
				"Deletion timed out waiting for VM %s to start. Clearing checkpoints — QEMU bitmaps may be orphaned and require manual cleanup.", vmiName)
			tracker.Status.Checkpoints = nil
			tracker.Status.LatestCheckpoint = nil
			return ctrl.removeTrackerFinalizer(tracker)
		}
		return fmt.Errorf("VMI %s/%s not found (VM exists but stopped), cannot clean up bitmaps — requeuing", tracker.Namespace, vmiName)
	}

	if !vmi.IsRunning() {
		if time.Since(tracker.DeletionTimestamp.Time) > forceDeleteTimeout(tracker) {
			logger.Warningf("Tracker %s/%s deletion timed out waiting for VMI %s to become running — clearing checkpoints (bitmaps may be orphaned)",
				tracker.Namespace, tracker.Name, vmiName)
			ctrl.recorder.Eventf(tracker, corev1.EventTypeWarning, "ForceDeleteTimeout",
				"Deletion timed out waiting for VMI %s to become running. Clearing checkpoints — QEMU bitmaps may be orphaned and require manual cleanup.", vmiName)
			tracker.Status.Checkpoints = nil
			tracker.Status.LatestCheckpoint = nil
			return ctrl.removeTrackerFinalizer(tracker)
		}
		return fmt.Errorf("VMI %s/%s is not running, cannot clean up bitmaps — requeuing", tracker.Namespace, vmiName)
	}

	if migrations.IsMigrating(vmi) {
		return fmt.Errorf("VMI %s/%s is migrating, deferring tracker deletion cleanup", tracker.Namespace, vmiName)
	}

	if !HasCBTStateEnabled(vmi.Status.ChangedBlockTracking) {
		logger.Infof("CBT not enabled for VMI %s/%s, clearing checkpoints without bitmap cleanup", tracker.Namespace, vmiName)
		tracker.Status.Checkpoints = nil
		tracker.Status.LatestCheckpoint = nil
		return ctrl.removeTrackerFinalizer(tracker)
	}

	ctrl.recorder.Eventf(tracker, corev1.EventTypeNormal, "TrackerDeletionCleanup",
		"Starting bitmap cleanup for %d checkpoints", len(tracker.Status.Checkpoints))

	totalCheckpoints := len(tracker.Status.Checkpoints)
	deleted := make(map[int]bool)
	for i := len(tracker.Status.Checkpoints) - 1; i >= 0; i-- {
		cp := tracker.Status.Checkpoints[i]
		logger.Infof("Deleting checkpoint %s from VMI %s during tracker deletion", cp.Name, vmiName)
		if err := ctrl.client.VirtualMachineInstance(tracker.Namespace).DeleteCheckpoint(
			context.Background(), vmiName, cp.Name); err != nil {
			logger.Reason(err).Warningf("DeleteCheckpoint failed for %s during tracker deletion, will retry", cp.Name)
		} else {
			deleted[i] = true
		}
	}
	var remaining []backupv1.BackupCheckpoint
	for i, cp := range tracker.Status.Checkpoints {
		if !deleted[i] {
			remaining = append(remaining, cp)
		}
	}

	if len(remaining) > 0 {
		tracker.Status.Checkpoints = remaining
		if len(tracker.Status.Checkpoints) > 0 {
			tracker.Status.LatestCheckpoint = &tracker.Status.Checkpoints[len(tracker.Status.Checkpoints)-1]
		} else {
			tracker.Status.LatestCheckpoint = nil
		}
		ctrl.recorder.Eventf(tracker, corev1.EventTypeWarning, "TrackerDeletionCleanupPartial",
			"%d of %d checkpoint deletions failed, will retry on next reconcile",
			len(remaining), totalCheckpoints)
		return fmt.Errorf("failed to delete %d checkpoints for tracker %s/%s, requeuing",
			len(remaining), tracker.Namespace, tracker.Name)
	}

	tracker.Status.Checkpoints = nil
	tracker.Status.LatestCheckpoint = nil
	return ctrl.removeTrackerFinalizer(tracker)
}

func (ctrl *VMBackupController) trackerHasActiveBackups(tracker *backupv1.VirtualMachineBackupTracker) (bool, error) {
	trackerKey := fmt.Sprintf("%s/%s", tracker.Namespace, tracker.Name)
	backupKeys, err := ctrl.backupInformer.GetIndexer().IndexKeys("backupTracker", trackerKey)
	if err != nil {
		return false, err
	}

	for _, key := range backupKeys {
		obj, exists, err := ctrl.backupInformer.GetStore().GetByKey(key)
		if err != nil {
			return false, fmt.Errorf("failed to get backup %s from store: %w", key, err)
		}
		if !exists {
			continue
		}
		backup, ok := obj.(*backupv1.VirtualMachineBackup)
		if !ok {
			continue
		}
		if !IsBackupTerminal(backup) {
			return true, nil
		}
	}

	return false, nil
}

func (ctrl *VMBackupController) updateBackupTracker(namespace string, tracker *backupv1.VirtualMachineBackupTracker, backupType backupv1.BackupType, backupStatus *v1.VirtualMachineInstanceBackupStatus, clearExisting bool) error {
	if tracker == nil {
		return nil
	}

	trackerCopy := tracker.DeepCopy()
	if trackerCopy.Status == nil {
		trackerCopy.Status = &backupv1.VirtualMachineBackupTrackerStatus{}
	}

	if clearExisting {
		trackerCopy.Status.Checkpoints = nil
	}

	newCp := backupv1.BackupCheckpoint{
		Name:         *backupStatus.CheckpointName,
		CreationTime: backupStatus.StartTimestamp,
		Type:         backupType,
		Volumes:      toVolumeNames(backupStatus.Volumes),
	}
	if slices.ContainsFunc(trackerCopy.Status.Checkpoints, func(cp backupv1.BackupCheckpoint) bool { return cp.Name == newCp.Name }) {
		return nil
	}

	trackerCopy.Status.Checkpoints = append(trackerCopy.Status.Checkpoints, newCp)
	if len(trackerCopy.Status.Checkpoints) > 0 {
		trackerCopy.Status.LatestCheckpoint = &trackerCopy.Status.Checkpoints[len(trackerCopy.Status.Checkpoints)-1]
	} else {
		trackerCopy.Status.LatestCheckpoint = nil
	}

	_, err := ctrl.client.VirtualMachineBackupTracker(namespace).UpdateStatus(
		context.Background(), trackerCopy, metav1.UpdateOptions{})
	if err != nil {
		return fmt.Errorf("failed to update BackupTracker status: %w", err)
	}

	log.Log.Infof("Successfully updated BackupTracker %s/%s with checkpoint %s (type=%s)",
		namespace, tracker.Name, newCp.Name, newCp.Type)
	return nil
}
