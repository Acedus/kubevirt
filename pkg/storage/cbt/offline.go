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

	"k8s.io/apimachinery/pkg/api/equality"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/cache"

	backupv1 "kubevirt.io/api/backup/v1alpha1"
	v1 "kubevirt.io/api/core/v1"
	"kubevirt.io/client-go/log"

	"kubevirt.io/kubevirt/pkg/controller"
)

const (
	vmHeldForOfflineBackupMsg = "vm %s is held for an offline backup"

	OfflineBackupPrepareContainerName = "backup-prepare"
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
