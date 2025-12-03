package export

import (
	"fmt"
	"path"
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
	updateStatus          func(vmExport *exportv1.VirtualMachineExport, exporterPod *corev1.Pod, service *corev1.Service, vmBackup *backupv1.VirtualMachineBackup, availableMessage string, internalLinks, externalLinks *exportv1.VirtualMachineExportLink) (time.Duration, error)
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
	for index, volume := range h.vmBackup.Status.IncludedVolumes {
		pod.Spec.Containers[0].Env = append(pod.Spec.Containers[0].Env, corev1.EnvVar{
			Name:  fmt.Sprintf("BACKUP%d_BACKUP_PATH", index),
			Value: volume,
		}, corev1.EnvVar{
			Name:  fmt.Sprintf("BACKUP%d_DATA_URI", index),
			Value: backupDataURI(volume),
		}, corev1.EnvVar{
			Name:  fmt.Sprintf("BACKUP%d_MAP_URI", index),
			Value: backupMapURI(volume),
		})
	}
}

func (h *backupSourceHandler) AvailableMessage() string {
	for _, cond := range h.vmBackup.Status.Conditions {
		if cond.Status == corev1.ConditionTrue {
			return cond.Message
		}
	}
	return ""
}

func (h *backupSourceHandler) UpdateStatus(vmExport *exportv1.VirtualMachineExport, pod *corev1.Pod, svc *corev1.Service, internalCert, externalCert, externalLinkHost string) (time.Duration, error) {
	internalLinks, err := h.GetInteralLinks(vmExport, pod, svc, internalCert)
	if err != nil {
		return 0, err
	}

	externalLinks, err := h.GetExternalLinks(vmExport, pod, externalLinkHost, externalCert)
	if err != nil {
		return 0, err
	}

	return h.updateStatus(vmExport, pod, svc, h.vmBackup, h.AvailableMessage(), internalLinks, externalLinks)
}

func (h *backupSourceHandler) GetExternalLinks(vmExport *exportv1.VirtualMachineExport, pod *corev1.Pod, externalLinkHost, cert string) (*exportv1.VirtualMachineExportLink, error) {
	urlPath := fmt.Sprintf(externalUrlLinkFormat, vmExport.Namespace, vmExport.Name)
	if externalLinkHost != "" {
		hostAndBase := path.Join(externalLinkHost, urlPath)
		return h.GetLinks(vmExport, pod, hostAndBase, external, cert)
	}
	return nil, nil
}

func (h *backupSourceHandler) GetInteralLinks(vmExport *exportv1.VirtualMachineExport, pod *corev1.Pod, svc *corev1.Service, internalCert string) (*exportv1.VirtualMachineExportLink, error) {
	host := fmt.Sprintf("%s.%s.svc", svc.Name, svc.Namespace)
	return h.GetLinks(vmExport, pod, host, internal, internalCert)
}

func (h *backupSourceHandler) GetLinks(vmExport *exportv1.VirtualMachineExport, pod *corev1.Pod, hostAndBase, linkType, cert string) (*exportv1.VirtualMachineExportLink, error) {
	const scheme = "https://"
	if pod == nil {
		return nil, nil
	}

	paths := CreateServerPaths(ContainerEnvToMap(pod.Spec.Containers[0].Env))
	exportLink, err := getLinks(paths, scheme, pod, vmExport, hostAndBase, linkType, cert)
	if err != nil {
		return nil, err
	}

	if h.vmBackup.Status == nil {
		return nil, err
	}

	for _, volume := range h.vmBackup.Status.IncludedVolumes {

		backupInfo := paths.GetBackupInfo(volume)
		if backupInfo == nil {
			log.Log.Warningf("Backup %s not found in paths", volume)
			continue
		}

		eb := exportv1.VirtualMachineExportBackup{
			Name: volume,
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
			log.Log.Warningf("No formats found for backup %s", volume)
			continue
		}

		exportLink.Backups = append(exportLink.Backups, eb)
	}
	return exportLink, nil
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

func (ctrl *VMExportController) updateVMBackupExportStatus(vmExport *exportv1.VirtualMachineExport, exporterPod *corev1.Pod, service *corev1.Service, vmBackup *backupv1.VirtualMachineBackup, availableMessage string, internalLinks, externalLinks *exportv1.VirtualMachineExportLink) (time.Duration, error) {
	var requeue time.Duration

	vmExportCopy := vmExport.DeepCopy()
	vmExportCopy.Status.VirtualMachineBackupName = pointer.P(vmBackup.Name)
	ctrl.updateCommonVMExportStatusFields(vmExport, vmExportCopy, exporterPod, service, availableMessage, internalLinks, externalLinks)
	if err := ctrl.updateVMExportStatus(vmExport, vmExportCopy); err != nil {
		return requeue, err
	}
	return requeue, nil
}
