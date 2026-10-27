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

package export

import (
	"fmt"
	"path"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	"k8s.io/client-go/tools/cache"

	exportv1 "kubevirt.io/api/export/v1"
	"kubevirt.io/client-go/log"

	backupv1 "kubevirt.io/api/backup/v1alpha1"
	virtv1 "kubevirt.io/api/core/v1"

	"kubevirt.io/kubevirt/pkg/apimachinery"
	"kubevirt.io/kubevirt/pkg/controller"
	backendstorage "kubevirt.io/kubevirt/pkg/storage/backend-storage"
	"kubevirt.io/kubevirt/pkg/storage/cbt"
	"kubevirt.io/kubevirt/pkg/storage/types"
)

const (
	backupsBasePath                = "/exports"
	vmBackupReadyReason            = "VirtualMachineBackupReady"
	vmBackupNotReadyMessage        = "VMBackup is not progressing"
	vmBackupNotExist               = "VMBackup does not exist yet"
	vmBackupNoProgressingCondition = "VMBackup progressing condition not found"
	vmBackupNoContent              = "VMBackup has no included volumes"
	vmBackupNoVMI                  = "VirtualMachineInstance of the VMBackup does not exist"
	vmBackupNoBackendStorage       = "VirtualMachineInstance of the VMBackup has no backend storage to serve the backup from"

	nbdVolumeName = "backup-nbd"
	nbdDir        = "/var/run/kubevirt/backup-nbd"
	nbdSocketPath = nbdDir + "/" + cbt.NBDSocketName
)

type VMBackupSource struct {
	vmBackup *backupv1.VirtualMachineBackup
	vmi      *virtv1.VirtualMachineInstance
	offline  *offlineBackup
}

func NewVMBackupSource(vmBackup *backupv1.VirtualMachineBackup, vmi *virtv1.VirtualMachineInstance) *VMBackupSource {
	return &VMBackupSource{
		vmBackup: vmBackup,
		vmi:      vmi,
	}
}

func (s *VMBackupSource) IsSourceAvailable() bool {
	return s.vmBackup != nil &&
		s.vmBackup.Status != nil &&
		meta.IsStatusConditionTrue(s.vmBackup.Status.Conditions, string(backupv1.ConditionProgressing)) &&
		(s.offline != nil || s.vmi != nil && backendstorage.CurrentPVCName(s.vmi) != "")
}

func (s *VMBackupSource) isOffline() bool {
	return s.offline != nil
}

func (s *VMBackupSource) isOfflinePush() bool {
	return s.isOffline() && s.vmBackup.IsPushMode()
}

func (s *VMBackupSource) HasContent() bool {
	return s.vmBackup.Status != nil && len(s.vmBackup.Status.IncludedVolumes) > 0
}

func (s *VMBackupSource) SourceCondition() exportv1.Condition {
	if s.vmBackup == nil {
		return newReadyCondition(corev1.ConditionFalse, vmBackupReadyReason, vmBackupNotExist)
	}
	if s.vmBackup.Status == nil {
		return newReadyCondition(corev1.ConditionFalse, vmBackupReadyReason, vmBackupNotReadyMessage)
	}
	if !s.HasContent() {
		return newReadyCondition(corev1.ConditionFalse, vmBackupReadyReason, vmBackupNoContent)
	}
	cond := meta.FindStatusCondition(s.vmBackup.Status.Conditions, string(backupv1.ConditionProgressing))
	if cond == nil {
		return newReadyCondition(corev1.ConditionFalse, vmBackupReadyReason, vmBackupNoProgressingCondition)
	}
	if cond.Status == metav1.ConditionFalse {
		return newReadyCondition(corev1.ConditionFalse, vmBackupReadyReason, cond.Message)
	}
	if !s.isOffline() {
		if s.vmi == nil {
			return newReadyCondition(corev1.ConditionFalse, vmBackupReadyReason, vmBackupNoVMI)
		}
		if backendstorage.CurrentPVCName(s.vmi) == "" {
			return newReadyCondition(corev1.ConditionFalse, vmBackupReadyReason, vmBackupNoBackendStorage)
		}
	}
	return newReadyCondition(corev1.ConditionTrue, vmBackupReadyReason, cond.Message)
}

func (s *VMBackupSource) ReadyCondition() exportv1.Condition {
	if !s.IsSourceAvailable() || !s.HasContent() {
		return newReadyCondition(corev1.ConditionFalse, initializingReason, "")
	}
	return exportv1.Condition{}
}

func (s *VMBackupSource) ConfigurePod(pod *corev1.Pod) {
	for index, volume := range s.vmBackup.Status.IncludedVolumes {
		pod.Spec.Containers[0].Env = append(pod.Spec.Containers[0].Env, corev1.EnvVar{
			Name:  fmt.Sprintf("BACKUP%d_BACKUP_PATH", index),
			Value: volume.VolumeName,
		}, corev1.EnvVar{
			Name:  fmt.Sprintf("BACKUP%d_DATA_URI", index),
			Value: backupDataURI(volume.VolumeName),
		}, corev1.EnvVar{
			Name:  fmt.Sprintf("BACKUP%d_MAP_URI", index),
			Value: backupMapURI(volume.VolumeName),
		}, corev1.EnvVar{
			Name:  fmt.Sprintf("BACKUP%d_TYPE", index),
			Value: string(volume.Type),
		})
	}
	if s.vmBackup.Status != nil {
		if s.vmBackup.Status.CheckpointName != nil {
			pod.Spec.Containers[0].Env = append(pod.Spec.Containers[0].Env, corev1.EnvVar{
				Name:  "BACKUP_CHECKPOINT",
				Value: *s.vmBackup.Status.CheckpointName,
			})
		}
	}

	if s.isOffline() {
		s.offline.configurePod(pod, s.vmBackup)
		return
	}

	pod.Spec.Containers[0].Env = append(pod.Spec.Containers[0].Env, corev1.EnvVar{
		Name:  "BACKUP_NBD_SOCKET",
		Value: nbdSocketPath,
	})

	s.configurePodAffinity(pod)
	s.configurePodTolerations(pod)
	s.configurePodNBDVolume(pod)
}

func (s *VMBackupSource) configurePodAffinity(pod *corev1.Pod) {
	if pod.Spec.Affinity == nil {
		pod.Spec.Affinity = &corev1.Affinity{}
	}
	if pod.Spec.Affinity.PodAffinity == nil {
		pod.Spec.Affinity.PodAffinity = &corev1.PodAffinity{}
	}
	pod.Spec.Affinity.PodAffinity.RequiredDuringSchedulingIgnoredDuringExecution = append(
		pod.Spec.Affinity.PodAffinity.RequiredDuringSchedulingIgnoredDuringExecution,
		corev1.PodAffinityTerm{
			LabelSelector: &metav1.LabelSelector{
				MatchLabels: map[string]string{
					virtv1.AppLabel:                      "virt-launcher",
					virtv1.VirtualMachineInstanceIDLabel: apimachinery.CalculateVirtualMachineInstanceID(s.vmi.Name),
				},
			},
			TopologyKey: corev1.LabelHostname,
		},
	)
}

func (s *VMBackupSource) configurePodTolerations(pod *corev1.Pod) {
	pod.Spec.Tolerations = append(pod.Spec.Tolerations, s.vmi.Spec.Tolerations...)
}

func (s *VMBackupSource) configurePodNBDVolume(pod *corev1.Pod) {
	pod.Spec.Volumes = append(pod.Spec.Volumes, corev1.Volume{
		Name: nbdVolumeName,
		VolumeSource: corev1.VolumeSource{
			PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{
				ClaimName: backendstorage.CurrentPVCName(s.vmi),
				ReadOnly:  true,
			},
		},
	})
	pod.Spec.Containers[0].VolumeMounts = append(pod.Spec.Containers[0].VolumeMounts, corev1.VolumeMount{
		Name:      nbdVolumeName,
		MountPath: nbdDir,
		SubPath:   path.Join(cbt.BackupNBDSubPath, cbt.NBDSocketDir),
		ReadOnly:  true,
	})
}

func (s *VMBackupSource) ConfigureExportLink(exportLink *exportv1.VirtualMachineExportLink, paths *ServerPaths, vmExport *exportv1.VirtualMachineExport, pod *corev1.Pod, hostAndBase, scheme string) {
	if pod == nil {
		return
	}

	if s.vmBackup.Status == nil || !s.HasContent() {
		return
	}

	for _, volume := range s.vmBackup.Status.IncludedVolumes {
		backupInfo := paths.GetBackupInfo(volume.VolumeName)
		if backupInfo == nil {
			log.Log.Warningf("Backup %s not found in paths", volume.VolumeName)
			continue
		}

		eb := exportv1.VirtualMachineExportBackup{
			Name: volume.VolumeName,
		}

		if backupInfo.DataURI != "" {
			eb.Endpoints = append(eb.Endpoints, exportv1.VirtualMachineExportBackupEndpoint{
				Endpoint: exportv1.Data,
				Url:      scheme + path.Join(hostAndBase, backupInfo.DataURI),
			})
		}

		if backupInfo.MapURI != "" {
			eb.Endpoints = append(eb.Endpoints, exportv1.VirtualMachineExportBackupEndpoint{
				Endpoint: exportv1.Map,
				Url:      scheme + path.Join(hostAndBase, backupInfo.MapURI),
			})
		}

		if len(eb.Endpoints) == 0 {
			log.Log.Warningf("No endpoints found for backup %s", volume.VolumeName)
			continue
		}

		exportLink.Backups = append(exportLink.Backups, eb)
	}
}

func (s *VMBackupSource) UpdateStatus(vmExport *exportv1.VirtualMachineExport, pod *corev1.Pod, svc *corev1.Service) (time.Duration, error) {
	var requeue time.Duration
	if !s.IsSourceAvailable() {
		log.Log.V(4).Infof("Source is not available %s, requeuing", s.SourceCondition().Message)
		requeue = requeueTime
	}

	vmExport.Status.Conditions = updateCondition(vmExport.Status.Conditions, s.SourceCondition())
	return requeue, nil
}

func (ctrl *VMExportController) handleVMBackup(obj any) {
	if unknown, ok := obj.(cache.DeletedFinalStateUnknown); ok && unknown.Obj != nil {
		obj = unknown.Obj
	}

	if backup, ok := obj.(*backupv1.VirtualMachineBackup); ok {
		backupKey, _ := cache.MetaNamespaceKeyFunc(backup)
		keys, err := ctrl.VMExportInformer.GetIndexer().IndexKeys("virtualmachinebackup", backupKey)
		if err != nil {
			utilruntime.HandleError(err)
			return
		}

		for _, key := range keys {
			log.Log.V(3).Infof("Adding VMExport due to backup %s", backupKey)
			ctrl.vmExportQueue.Add(key)
		}
	}
}

func (ctrl *VMExportController) isSourceBackup(source *exportv1.VirtualMachineExportSpec) bool {
	return source != nil && (source.Source.APIGroup == nil || *source.Source.APIGroup == backupv1.SchemeGroupVersion.Group) && source.Source.Kind == "VirtualMachineBackup"
}

func (ctrl *VMExportController) getBackup(namespace, name string) (*backupv1.VirtualMachineBackup, bool, error) {
	key := controller.NamespacedKey(namespace, name)
	obj, exists, err := ctrl.VMBackupInformer.GetStore().GetByKey(key)
	if err != nil || !exists {
		return nil, exists, err
	}
	return obj.(*backupv1.VirtualMachineBackup).DeepCopy(), true, nil
}

func (ctrl *VMExportController) getVMBackupFromExport(vmExport *exportv1.VirtualMachineExport) (*backupv1.VirtualMachineBackup, error) {
	vmBackup, exists, err := ctrl.getBackup(vmExport.Namespace, vmExport.Spec.Source.Name)
	if err != nil {
		return nil, fmt.Errorf("error fetching backup %s/%s: %w", vmExport.Namespace, vmExport.Spec.Source.Name, err)
	}
	if !exists {
		return nil, fmt.Errorf("VirtualMachineBackup not found: %s/%s", vmExport.Namespace, vmExport.Spec.Source.Name)
	}
	return vmBackup, nil
}

func backupMapURI(volumeName string) string {
	return path.Join(backupsBasePath, volumeName, "map")
}

func backupDataURI(volumeName string) string {
	return path.Join(backupsBasePath, volumeName, "data")
}

func (ctrl *VMExportController) getBackupTracker(namespace, name string) (*backupv1.VirtualMachineBackupTracker, bool, error) {
	key := controller.NamespacedKey(namespace, name)
	obj, exists, err := ctrl.VMBackupTrackerInformer.GetStore().GetByKey(key)
	if err != nil || !exists {
		return nil, exists, err
	}
	return obj.(*backupv1.VirtualMachineBackupTracker).DeepCopy(), true, nil
}

// getBackupSourceVMI resolves the VMI of the backup's source, handling both
// direct VM sources and VirtualMachineBackupTracker sources.
func (ctrl *VMExportController) getBackupSourceVMI(vmBackup *backupv1.VirtualMachineBackup) (*virtv1.VirtualMachineInstance, error) {
	var vmName string

	if vmBackup.Spec.Source.Kind == backupv1.VirtualMachineBackupTrackerGroupVersionKind.Kind {
		tracker, exists, err := ctrl.getBackupTracker(vmBackup.Namespace, vmBackup.Spec.Source.Name)
		if err != nil {
			return nil, fmt.Errorf("error fetching backup tracker %s/%s: %w", vmBackup.Namespace, vmBackup.Spec.Source.Name, err)
		}
		if !exists {
			return nil, fmt.Errorf("VirtualMachineBackupTracker not found: %s/%s", vmBackup.Namespace, vmBackup.Spec.Source.Name)
		}
		vmName = tracker.Spec.Source.Name
	} else {
		vmName = vmBackup.Spec.Source.Name
	}

	objKey := controller.NamespacedKey(vmBackup.Namespace, vmName)
	obj, exists, err := ctrl.VMIInformer.GetStore().GetByKey(objKey)
	if err != nil {
		return nil, fmt.Errorf("error fetching VMI %s: %w", objKey, err)
	}
	if !exists {
		return nil, nil
	}

	return obj.(*virtv1.VirtualMachineInstance), nil
}

const (
	offlineStateMountPath  = "/backup-state"
	offlineSocketDir       = "/sockets"
	offlineTargetMountPath = "/backup-target"
	offlineVolumesDir      = "/backup-volumes"
	offlineSocketVolume    = "backup-sockets"
	offlineStateVolume     = "backup-state"
	offlineTargetVolume    = "backup-target"

	backupTimeFormat = "2006-01-02_15-04-05"
)

type offlineBackup struct {
	vmName         string
	dataPVCs       map[string]*corev1.PersistentVolumeClaim
	statePVC       *corev1.PersistentVolumeClaim
	targetPVC      *corev1.PersistentVolumeClaim
	baseCheckpoint string
}

// OfflineBackupVolumePath is where the exporter pod finds the data of an offline backup's volume.
func OfflineBackupVolumePath(volumeName string) string {
	return path.Join(offlineVolumesDir, volumeName)
}

func NewOfflineVMBackupSource(vmBackup *backupv1.VirtualMachineBackup, vmName string, dataPVCs map[string]*corev1.PersistentVolumeClaim, statePVC, targetPVC *corev1.PersistentVolumeClaim, baseCheckpoint string) *VMBackupSource {
	return &VMBackupSource{
		vmBackup: vmBackup,
		offline: &offlineBackup{
			vmName:         vmName,
			dataPVCs:       dataPVCs,
			statePVC:       statePVC,
			targetPVC:      targetPVC,
			baseCheckpoint: baseCheckpoint,
		},
	}
}

func (o *offlineBackup) targetDir(vmBackup *backupv1.VirtualMachineBackup) string {
	backupTime := vmBackup.CreationTimestamp.UTC().Format(backupTimeFormat)
	return fmt.Sprintf("%s/%s/%s-%s", offlineTargetMountPath, o.vmName, vmBackup.Name, backupTime)
}

func (o *offlineBackup) configurePod(pod *corev1.Pod, vmBackup *backupv1.VirtualMachineBackup) {
	container := &pod.Spec.Containers[0]

	mountPVC(pod, container, offlineStateVolume, o.statePVC, offlineStateMountPath)

	pod.Spec.Volumes = append(pod.Spec.Volumes, corev1.Volume{
		Name:         offlineSocketVolume,
		VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{}},
	})
	container.VolumeMounts = append(container.VolumeMounts, corev1.VolumeMount{
		Name:      offlineSocketVolume,
		MountPath: offlineSocketDir,
	})

	container.Env = append(container.Env,
		corev1.EnvVar{Name: "OFFLINE_BACKUP", Value: "true"},
		corev1.EnvVar{Name: "BACKUP_STATE_PATH", Value: offlineStateMountPath},
		corev1.EnvVar{Name: "SOCKET_DIR", Value: offlineSocketDir},
	)
	if o.baseCheckpoint != "" {
		container.Env = append(container.Env, corev1.EnvVar{Name: "BACKUP_BASE_CHECKPOINT", Value: o.baseCheckpoint})
	}

	if vmBackup.IsPushMode() && o.targetPVC != nil {
		mountPVC(pod, container, offlineTargetVolume, o.targetPVC, offlineTargetMountPath)
		container.Env = append(container.Env,
			corev1.EnvVar{Name: "BACKUP_MODE", Value: string(backupv1.PushMode)},
			corev1.EnvVar{Name: "BACKUP_TARGET_DIR", Value: o.targetDir(vmBackup)},
			corev1.EnvVar{Name: "BACKUP_NAME", Value: vmBackup.Name},
		)
	}

	for index, volume := range vmBackup.Status.IncludedVolumes {
		if pvc := o.dataPVCs[volume.VolumeName]; pvc != nil {
			mountPVC(pod, container, fmt.Sprintf("backup-vol%d", index), pvc, OfflineBackupVolumePath(volume.VolumeName))
		}
	}
}

// always RW: qemu-img bitmap --add/--remove needs write access to the data disk too
func mountPVC(pod *corev1.Pod, container *corev1.Container, volumeName string, pvc *corev1.PersistentVolumeClaim, mountPath string) {
	pod.Spec.Volumes = append(pod.Spec.Volumes, corev1.Volume{
		Name: volumeName,
		VolumeSource: corev1.VolumeSource{
			PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{ClaimName: pvc.Name},
		},
	})
	if types.IsPVCBlock(pvc.Spec.VolumeMode) {
		container.VolumeDevices = append(container.VolumeDevices, corev1.VolumeDevice{
			Name:       volumeName,
			DevicePath: mountPath,
		})
		return
	}
	container.VolumeMounts = append(container.VolumeMounts, corev1.VolumeMount{
		Name:      volumeName,
		MountPath: mountPath,
	})
}

func (ctrl *VMExportController) isOfflineBackup(vmBackup *backupv1.VirtualMachineBackup) (bool, error) {
	vmName, err := ctrl.getBackupSourceVMName(vmBackup)
	if err != nil {
		return false, err
	}
	vm, exists, err := ctrl.getVm(vmBackup.Namespace, vmName)
	if err != nil || !exists {
		return false, err
	}
	return vm.Status.BackupInProgress != nil && *vm.Status.BackupInProgress == vmBackup.Name, nil
}

func (ctrl *VMExportController) getOfflineBackupSource(vmExport *exportv1.VirtualMachineExport, vmBackup *backupv1.VirtualMachineBackup) (*VMBackupSource, error) {
	vmName, err := ctrl.getBackupSourceVMName(vmBackup)
	if err != nil {
		return nil, err
	}

	statePVC, err := ctrl.findStatePVC(vmExport.Namespace, vmName)
	if err != nil {
		return nil, err
	}
	if statePVC == nil {
		return nil, fmt.Errorf("state PVC of VM %s/%s is not known yet, start the VM once to record it", vmExport.Namespace, vmName)
	}

	dataPVCs, err := ctrl.resolveOfflineDataPVCs(vmExport.Namespace, vmName, vmBackup)
	if err != nil {
		return nil, err
	}

	baseCheckpoint := ""
	if vmBackup.Spec.Source.Kind == backupv1.VirtualMachineBackupTrackerGroupVersionKind.Kind {
		tracker, _, err := ctrl.getBackupTracker(vmExport.Namespace, vmBackup.Spec.Source.Name)
		if err != nil {
			return nil, err
		}
		if cbt.IsIncrementalBackup(vmBackup, tracker) {
			baseCheckpoint = tracker.Status.LatestCheckpoint.Name
		}
	}

	var targetPVC *corev1.PersistentVolumeClaim
	if vmBackup.IsPushMode() {
		if vmBackup.Spec.PvcName == nil {
			return nil, fmt.Errorf("push mode offline backup %s/%s has no target PVC", vmBackup.Namespace, vmBackup.Name)
		}
		pvc, exists, err := ctrl.getPvc(vmExport.Namespace, *vmBackup.Spec.PvcName)
		if err != nil {
			return nil, err
		}
		if !exists {
			return nil, fmt.Errorf("target PVC %s/%s not found", vmExport.Namespace, *vmBackup.Spec.PvcName)
		}
		targetPVC = pvc
	}

	return NewOfflineVMBackupSource(vmBackup, vmName, dataPVCs, statePVC, targetPVC, baseCheckpoint), nil
}

func (ctrl *VMExportController) findStatePVC(namespace, vmName string) (*corev1.PersistentVolumeClaim, error) {
	vm, exists, err := ctrl.getVm(namespace, vmName)
	if err != nil || !exists {
		return nil, err
	}
	state := vm.Status.VirtualMachineStateVolume
	if state == nil || state.PersistentVolumeClaimInfo == nil {
		return nil, nil
	}
	pvc, exists, err := ctrl.getPvc(namespace, state.PersistentVolumeClaimInfo.ClaimName)
	if err != nil || !exists {
		return nil, err
	}
	return pvc, nil
}

func (ctrl *VMExportController) resolveOfflineDataPVCs(namespace, vmName string, vmBackup *backupv1.VirtualMachineBackup) (map[string]*corev1.PersistentVolumeClaim, error) {
	vm, exists, err := ctrl.getVm(namespace, vmName)
	if err != nil {
		return nil, err
	}
	if !exists {
		return nil, fmt.Errorf("VM %s/%s not found", namespace, vmName)
	}
	if vm.Spec.Template == nil {
		return nil, fmt.Errorf("VM %s/%s has no template", namespace, vmName)
	}

	included := map[string]bool{}
	for _, v := range vmBackup.Status.IncludedVolumes {
		included[v.VolumeName] = true
	}

	dataPVCs := map[string]*corev1.PersistentVolumeClaim{}
	for i := range vm.Spec.Template.Spec.Volumes {
		volume := vm.Spec.Template.Spec.Volumes[i]
		if !included[volume.Name] {
			continue
		}
		pvcName := types.PVCNameFromVirtVolume(&volume)
		if pvcName == "" {
			continue
		}
		pvc, exists, err := ctrl.getPvc(namespace, pvcName)
		if err != nil {
			return nil, err
		}
		if !exists {
			return nil, fmt.Errorf("data PVC %s/%s for volume %s not found", namespace, pvcName, volume.Name)
		}
		dataPVCs[volume.Name] = pvc
	}
	return dataPVCs, nil
}

func (ctrl *VMExportController) getBackupSourceVMName(vmBackup *backupv1.VirtualMachineBackup) (string, error) {
	vmName, err := cbt.BackupSourceVMName(ctrl.VMBackupTrackerInformer.GetStore(), vmBackup)
	if err == nil && vmName == "" {
		err = fmt.Errorf("VirtualMachineBackupTracker not found: %s/%s", vmBackup.Namespace, vmBackup.Spec.Source.Name)
	}
	return vmName, err
}
