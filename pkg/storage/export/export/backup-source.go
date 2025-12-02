package export

import (
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	"k8s.io/client-go/tools/cache"

	backupv1 "kubevirt.io/api/backup/v1alpha1"
	exportv1 "kubevirt.io/api/export/v1beta1"
	"kubevirt.io/client-go/log"

	"kubevirt.io/kubevirt/pkg/controller"
	"kubevirt.io/kubevirt/pkg/pointer"
)

type backupSourceHandler struct {
	vmBackup              *backupv1.VirtualMachineBackup
	isKubevirtContentType contentTypeChecker
	updateStatus          func(vmExport *exportv1.VirtualMachineExport, exporterPod *corev1.Pod, service *corev1.Service, vmBackup *backupv1.VirtualMachineBackup, availableMessage string) (time.Duration, error)
}

func (ctrl *VMExportController) newBackupSourceHandler(vmBackup *backupv1.VirtualMachineBackup) *backupSourceHandler {
	return &backupSourceHandler{
		vmBackup:              vmBackup,
		isKubevirtContentType: ctrl.isKubevirtContentType,
		updateStatus:          ctrl.updateVMBackupExportStatus,
	}
}

func (h *backupSourceHandler) IsSourceAvailable() bool {
	return h.hasCondition(backupv1.ConditionReadyToPull)
}

func (h *backupSourceHandler) hasCondition(condType backupv1.ConditionType) bool {
	if h.vmBackup.Status == nil {
		return false
	}
	for _, cond := range h.vmBackup.Status.Conditions {
		if cond.Type == condType {
			return cond.Status == corev1.ConditionTrue
		}
	}
	return false
}

func (h *backupSourceHandler) HasContent() bool {
	return true
}

func (h *backupSourceHandler) Ports() []corev1.ServicePort {
	return []corev1.ServicePort{
		exportPort(),
		backupPort(),
	}
}

func (h *backupSourceHandler) ConfigurePodManifest(pod *corev1.Pod) {
	pod.Spec.Containers[0].Env = append(pod.Spec.Containers[0].Env, corev1.EnvVar{
		Name:  "EXPORT_BACKUP_DEF_URI",
		Value: backupPath,
	})
}

func (h *backupSourceHandler) AvailableMessage() string {
	for _, cond := range h.vmBackup.Status.Conditions {
		if cond.Status == corev1.ConditionTrue {
			return cond.Message
		}
	}
	return ""
}

func (h *backupSourceHandler) UpdateStatus(vmExport *exportv1.VirtualMachineExport, pod *corev1.Pod, svc *corev1.Service) (time.Duration, error) {
	return h.updateStatus(vmExport, pod, svc, h.vmBackup, h.AvailableMessage())
}

func backupPort() corev1.ServicePort {
	return corev1.ServicePort{
		Name:     "backup",
		Protocol: "TCP",
		Port:     9090,
		TargetPort: intstr.IntOrString{
			Type:   intstr.Int,
			IntVal: 9090,
		},
	}
}

func (ctrl *VMExportController) handleVMBackup(obj interface{}) {
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

func (ctrl *VMExportController) updateVMBackupExportStatus(vmExport *exportv1.VirtualMachineExport, exporterPod *corev1.Pod, service *corev1.Service, vmBackup *backupv1.VirtualMachineBackup, availableMessage string) (time.Duration, error) {
	var requeue time.Duration

	vmExportCopy := vmExport.DeepCopy()
	vmExportCopy.Status.VirtualMachineBackupName = pointer.P(vmBackup.Name)
	if err := ctrl.updateCommonVMExportStatusFields(vmExport, vmExportCopy, exporterPod, service, availableMessage, nil, getVolumeName); err != nil {
		return requeue, err
	}

	if err := ctrl.updateVMExportStatus(vmExport, vmExportCopy); err != nil {
		return requeue, err
	}
	return requeue, nil
}
