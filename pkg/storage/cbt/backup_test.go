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

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"go.uber.org/mock/gomock"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	k8stypes "k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes/fake"
	"k8s.io/client-go/testing"
	"k8s.io/client-go/tools/cache"
	framework "k8s.io/client-go/tools/cache/testing"
	"k8s.io/client-go/tools/record"
	"k8s.io/client-go/util/workqueue"

	backupv1 "kubevirt.io/api/backup/v1alpha1"
	v1 "kubevirt.io/api/core/v1"
	exportv1 "kubevirt.io/api/export/v1"
	"kubevirt.io/client-go/kubecli"
	kubevirtfake "kubevirt.io/client-go/kubevirt/fake"

	"kubevirt.io/kubevirt/pkg/pointer"
	"kubevirt.io/kubevirt/pkg/testutils"
)

const (
	testNamespace     = "default"
	vmName            = "test-vm"
	backupName        = "test-backup"
	backupTrackerName = "test-backup-tracker"
	checkpointName    = "test-checkpoint"
	pvcName           = "backup-target"
)

var (
	vmUID     k8stypes.UID = "vm-uid"
	backupUID k8stypes.UID = "backup-uid"
)

func newCondition(condType string, status metav1.ConditionStatus, reason, message string) metav1.Condition {
	return metav1.Condition{
		Type:               condType,
		Status:             status,
		Reason:             reason,
		Message:            message,
		LastTransitionTime: metav1.Now(),
	}
}

var _ = Describe("Backup Controller", func() {
	var (
		ctrl                  *gomock.Controller
		virtClient            *kubecli.MockKubevirtClient
		vmInterface           *kubecli.MockVirtualMachineInterface
		vmiInterface          *kubecli.MockVirtualMachineInstanceInterface
		backupInformer        cache.SharedIndexInformer
		backupSource          *framework.FakeControllerSource
		backupTrackerInformer cache.SharedIndexInformer
		vmInformer            cache.SharedIndexInformer
		vmiInformer           cache.SharedIndexInformer
		pvcInformer           cache.SharedIndexInformer
		vmExportInformer      cache.SharedIndexInformer
		controller            *VMBackupController
		recorder              *record.FakeRecorder
		mockBackupQueue       *testutils.MockWorkQueue[string]

		kubevirtClient *kubevirtfake.Clientset
		k8sClient      *fake.Clientset
	)

	createBackup := func(name, vmName, pvcName string, mode backupv1.BackupMode) *backupv1.VirtualMachineBackup {
		return &backupv1.VirtualMachineBackup{
			ObjectMeta: metav1.ObjectMeta{
				Name:      name,
				Namespace: testNamespace,
				UID:       backupUID,
			},
			Spec: backupv1.VirtualMachineBackupSpec{
				Source: corev1.TypedLocalObjectReference{
					APIGroup: pointer.P("kubevirt.io"),
					Kind:     "VirtualMachine",
					Name:     vmName,
				},
				PvcName: pointer.P(pvcName),
				Mode:    pointer.P(mode),
			},
			Status: &backupv1.VirtualMachineBackupStatus{},
		}
	}

	createBackupWithTracker := func(name, vmName, pvcName string) *backupv1.VirtualMachineBackup {
		backup := createBackup(name, vmName, pvcName, backupv1.PushMode)
		backup.Spec.Source = corev1.TypedLocalObjectReference{
			APIGroup: pointer.P("backup.kubevirt.io"),
			Kind:     backupv1.VirtualMachineBackupTrackerGroupVersionKind.Kind,
			Name:     backupTrackerName,
		}
		return backup
	}

	createVMI := func() *v1.VirtualMachineInstance {
		return &v1.VirtualMachineInstance{
			ObjectMeta: metav1.ObjectMeta{
				Name:      vmName,
				Namespace: testNamespace,
			},
			Spec: v1.VirtualMachineInstanceSpec{
				Volumes: []v1.Volume{
					{
						Name: "disk0",
						VolumeSource: v1.VolumeSource{
							PersistentVolumeClaim: &v1.PersistentVolumeClaimVolumeSource{
								PersistentVolumeClaimVolumeSource: corev1.PersistentVolumeClaimVolumeSource{
									ClaimName: "test-disk",
								},
							},
						},
					},
				},
			},
			Status: v1.VirtualMachineInstanceStatus{
				ChangedBlockTracking: &v1.ChangedBlockTrackingStatus{
					State: v1.ChangedBlockTrackingEnabled,
				},
				Phase: v1.Running,
			},
		}
	}

	createVMIWithPVCAttached := func() *v1.VirtualMachineInstance {
		vmi := createVMI()
		volumeName := backupTargetVolumeName(backupName)
		vmi.Spec.UtilityVolumes = []v1.UtilityVolume{
			{
				Name: volumeName,
				PersistentVolumeClaimVolumeSource: corev1.PersistentVolumeClaimVolumeSource{
					ClaimName: pvcName,
				},
				Type: pointer.P(v1.Backup),
			},
		}
		vmi.Status.VolumeStatus = []v1.VolumeStatus{
			{
				Name:          volumeName,
				Phase:         v1.HotplugVolumeMounted,
				HotplugVolume: &v1.HotplugVolumeStatus{},
			},
		}
		return vmi
	}

	createInitializedVMI := func() *v1.VirtualMachineInstance {
		vmi := createVMIWithPVCAttached()
		vmi.Status.ChangedBlockTracking.BackupStatus = &v1.VirtualMachineInstanceBackupStatus{
			BackupName:     backupName,
			Completed:      false,
			CheckpointName: pointer.P(checkpointName),
		}
		return vmi
	}

	createBackupTracker := func(name, vmName, checkpointName string) *backupv1.VirtualMachineBackupTracker {
		tracker := &backupv1.VirtualMachineBackupTracker{
			ObjectMeta: metav1.ObjectMeta{
				Name:      name,
				Namespace: testNamespace,
			},
			Spec: backupv1.VirtualMachineBackupTrackerSpec{
				Source: corev1.TypedLocalObjectReference{
					APIGroup: pointer.P("kubevirt.io"),
					Kind:     "VirtualMachine",
					Name:     vmName,
				},
			},
			Status: &backupv1.VirtualMachineBackupTrackerStatus{},
		}
		if checkpointName != "" {
			tracker.Status.LatestCheckpoint = &backupv1.BackupCheckpoint{
				Name:         checkpointName,
				CreationTime: &metav1.Time{Time: metav1.Now().Time},
			}
		}
		return tracker
	}

	createVM := func(name string) *v1.VirtualMachine {
		return &v1.VirtualMachine{
			ObjectMeta: metav1.ObjectMeta{
				Name:      name,
				Namespace: testNamespace,
				UID:       vmUID,
			},
		}
	}

	createPVC := func(name string) *corev1.PersistentVolumeClaim {
		return &corev1.PersistentVolumeClaim{
			ObjectMeta: metav1.ObjectMeta{
				Name:      name,
				Namespace: testNamespace,
			},
			Spec: corev1.PersistentVolumeClaimSpec{
				VolumeMode: pointer.P(corev1.PersistentVolumeFilesystem),
			},
		}
	}

	addBackup := func(backup *backupv1.VirtualMachineBackup) {
		backupSource.Add(backup)
		backupInformer.GetStore().Add(backup)
		_, err := kubevirtClient.BackupV1alpha1().VirtualMachineBackups(backup.Namespace).Get(context.Background(), backup.Name, metav1.GetOptions{})
		if err != nil {
			_, err = kubevirtClient.BackupV1alpha1().VirtualMachineBackups(backup.Namespace).Create(context.Background(), backup, metav1.CreateOptions{})
			Expect(err).ToNot(HaveOccurred())
		}
		key := fmt.Sprintf("%s/%s", backup.Namespace, backup.Name)
		controller.backupQueue.Add(key)
	}

	BeforeEach(func() {
		ctrl = gomock.NewController(GinkgoT())
		virtClient = kubecli.NewMockKubevirtClient(ctrl)
		vmInterface = kubecli.NewMockVirtualMachineInterface(ctrl)
		vmiInterface = kubecli.NewMockVirtualMachineInstanceInterface(ctrl)

		backupInformer, backupSource = testutils.NewFakeInformerWithIndexersFor(
			&backupv1.VirtualMachineBackup{},
			cache.Indexers{
				"vmi": func(obj interface{}) ([]string, error) {
					backup := obj.(*backupv1.VirtualMachineBackup)
					if backup.Spec.Source.Kind == v1.VirtualMachineGroupVersionKind.Kind {
						return []string{fmt.Sprintf("%s/%s", backup.Namespace, backup.Spec.Source.Name)}, nil
					}
					key := fmt.Sprintf("%s/%s", backup.Namespace, backup.Spec.Source.Name)
					return []string{key}, nil
				},
				"backupTracker": func(obj interface{}) ([]string, error) {
					backup := obj.(*backupv1.VirtualMachineBackup)
					if backup.Spec.Source.Kind == backupv1.VirtualMachineBackupTrackerGroupVersionKind.Kind {
						return []string{fmt.Sprintf("%s/%s", backup.Namespace, backup.Spec.Source.Name)}, nil
					}
					return nil, nil
				},
			},
		)
		backupTrackerInformer, _ = testutils.NewFakeInformerWithIndexersFor(
			&backupv1.VirtualMachineBackupTracker{},
			cache.Indexers{
				"vmi": func(obj interface{}) ([]string, error) {
					tracker := obj.(*backupv1.VirtualMachineBackupTracker)
					return []string{fmt.Sprintf("%s/%s", tracker.Namespace, tracker.Spec.Source.Name)}, nil
				},
			},
		)
		vmInformer, _ = testutils.NewFakeInformerFor(&v1.VirtualMachine{})
		vmiInformer, _ = testutils.NewFakeInformerFor(&v1.VirtualMachineInstance{})
		pvcInformer, _ = testutils.NewFakeInformerFor(&corev1.PersistentVolumeClaim{})
		vmExportInformer, _ = testutils.NewFakeInformerFor(&exportv1.VirtualMachineExport{})

		recorder = record.NewFakeRecorder(100)
		recorder.IncludeObject = true

		controller = &VMBackupController{
			client:                virtClient,
			backupInformer:        backupInformer,
			backupTrackerInformer: backupTrackerInformer,
			vmStore:               vmInformer.GetStore(),
			vmiStore:              vmiInformer.GetStore(),
			pvcStore:              pvcInformer.GetStore(),
			vmExportStore:         vmExportInformer.GetStore(),
			recorder:              recorder,
			backupQueue: workqueue.NewTypedRateLimitingQueueWithConfig(
				workqueue.DefaultTypedControllerRateLimiter[string](),
				workqueue.TypedRateLimitingQueueConfig[string]{Name: "test-backup-queue"},
			),
		}
		controller.hasSynced = func() bool {
			return backupInformer.HasSynced() && backupTrackerInformer.HasSynced() && vmInformer.HasSynced() && vmiInformer.HasSynced() && pvcInformer.HasSynced()
		}

		mockBackupQueue = testutils.NewMockWorkQueue(controller.backupQueue)
		controller.backupQueue = mockBackupQueue

		virtClient.EXPECT().VirtualMachine(testNamespace).Return(vmInterface).AnyTimes()
		virtClient.EXPECT().VirtualMachineInstance(testNamespace).Return(vmiInterface).AnyTimes()

		kubevirtClient = kubevirtfake.NewSimpleClientset()
		virtClient.EXPECT().VirtualMachineBackup(testNamespace).
			Return(kubevirtClient.BackupV1alpha1().VirtualMachineBackups(testNamespace)).AnyTimes()
		virtClient.EXPECT().VirtualMachineExport(testNamespace).
			Return(kubevirtClient.ExportV1().VirtualMachineExports(testNamespace)).AnyTimes()

		k8sClient = fake.NewSimpleClientset()
		virtClient.EXPECT().CoreV1().Return(k8sClient.CoreV1()).AnyTimes()
	})

	Context("Verify source name", func() {
		It("should fail when source name is empty", func() {
			backup := createBackup(backupName, vmName, pvcName, backupv1.PushMode)
			backup.Spec.Source.Name = ""

			addBackup(backup)
			err := controller.execute(fmt.Sprintf("%s/%s", testNamespace, backupName))
			Expect(err).To(HaveOccurred())
			Expect(err).To(MatchError(errSourceNameEmpty))
		})

		It("should get source name from backupTracker when source kind is VirtualMachineBackupTracker", func() {
			backupTracker := createBackupTracker(backupTrackerName, vmName, "")
			controller.backupTrackerInformer.GetStore().Add(backupTracker)

			backup := createBackupWithTracker(backupName, vmName, pvcName)
			backup.Finalizers = []string{vmBackupFinalizer}

			vm := createVM(vmName)
			controller.vmStore.Add(vm)

			vmi := createVMIWithPVCAttached()
			controller.vmiStore.Add(vmi)

			pvc := createPVC(pvcName)
			controller.pvcStore.Add(pvc)

			vmiInterface.EXPECT().
				Patch(gomock.Any(), vmName, k8stypes.JSONPatchType, gomock.Any(), gomock.Any()).
				Return(vmi, nil)

			backupCalled := false
			vmiInterface.EXPECT().
				Backup(gomock.Any(), vmName, gomock.Any()).
				DoAndReturn(func(ctx context.Context, name string, options *backupv1.BackupOptions) error {
					backupCalled = true
					Expect(options.BackupName).To(Equal(backupName))
					return nil
				})

			err := controller.sync(backup)
			Expect(err).ToNot(HaveOccurred())
			Expect(meta.IsStatusConditionTrue(backup.Status.Conditions, string(backupv1.ConditionProgressing))).To(BeTrue())
			Expect(backupCalled).To(BeTrue())
		})
	})

	It("should wait when backupTracker does not exist yet", func() {
		backup := createBackupWithTracker(backupName, vmName, pvcName)

		err := controller.sync(backup)
		Expect(err).ToNot(HaveOccurred())
		Expect(meta.IsStatusConditionTrue(backup.Status.Conditions, string(backupv1.ConditionInitializing))).To(BeTrue())
	})

	It("should wait when backupTracker needs checkpoint redefinition", func() {
		backupTracker := createBackupTracker(backupTrackerName, vmName, "existing-checkpoint")
		backupTracker.Status.CheckpointRedefinitionRequired = pointer.P(true)
		controller.backupTrackerInformer.GetStore().Add(backupTracker)

		backup := createBackupWithTracker(backupName, vmName, pvcName)

		vm := createVM(vmName)
		controller.vmStore.Add(vm)

		vmi := createVMI()
		controller.vmiStore.Add(vmi)

		err := controller.sync(backup)
		Expect(err).ToNot(HaveOccurred())
		cond := meta.FindStatusCondition(backup.Status.Conditions, string(backupv1.ConditionInitializing))
		Expect(cond).ToNot(BeNil())
		Expect(cond.Status).To(Equal(metav1.ConditionTrue))
		Expect(cond.Message).To(ContainSubstring(fmt.Sprintf(trackerCheckpointRedefinitionPending, backupTrackerName)))
	})

	Context("source verification", func() {
		Context("sourceVMExists", func() {
			It("should return false when VM doesn't exist", func() {
				backup := createBackup(backupName, vmName, pvcName, backupv1.PushMode)
				exists, err := controller.sourceVMExists(backup, vmName)
				Expect(err).ToNot(HaveOccurred())
				Expect(exists).To(BeFalse())
			})

			It("should return true when VM exists", func() {
				backup := createBackup(backupName, vmName, pvcName, backupv1.PushMode)
				vm := createVM(vmName)
				controller.vmStore.Add(vm)
				exists, err := controller.sourceVMExists(backup, vmName)
				Expect(err).ToNot(HaveOccurred())
				Expect(exists).To(BeTrue())
			})
		})

		Context("vmiFromSource", func() {
			It("should return false when VMI doesn't exist", func() {
				backup := createBackup(backupName, vmName, pvcName, backupv1.PushMode)
				vmi, exists, err := controller.vmiFromSource(backup, vmName)
				Expect(err).ToNot(HaveOccurred())
				Expect(exists).To(BeFalse())
				Expect(vmi).To(BeNil())
			})

			It("should return VMI when it exists", func() {
				backup := createBackup(backupName, vmName, pvcName, backupv1.PushMode)
				expectedVMI := createVMI()
				controller.vmiStore.Add(expectedVMI)
				vmi, exists, err := controller.vmiFromSource(backup, vmName)
				Expect(err).ToNot(HaveOccurred())
				Expect(exists).To(BeTrue())
				Expect(vmi).To(Equal(expectedVMI))
			})
		})

		Context("verifyVMIEligibleForBackup", func() {
			It("should fail when VMI doesn't have CBT eligible volumes", func() {
				vmi := createVMI()
				vmi.Spec.Volumes = []v1.Volume{}
				reason := controller.verifyVMIEligibleForBackup(vmi)
				Expect(reason).ToNot(BeEmpty())
				Expect(reason).To(Equal(fmt.Sprintf(vmNoVolumesToBackupMsg, vmName)))
			})

			It("should fail when VMI doesn't have ChangedBlockTracking", func() {
				vmi := createVMI()
				vmi.Status.ChangedBlockTracking = nil
				reason := controller.verifyVMIEligibleForBackup(vmi)
				Expect(reason).ToNot(BeEmpty())
				Expect(reason).To(Equal(fmt.Sprintf(vmNoChangedBlockTrackingMsg, vmName)))
			})

			It("should fail when ChangedBlockTracking is not enabled", func() {
				vmi := createVMI()
				vmi.Status.ChangedBlockTracking = &v1.ChangedBlockTrackingStatus{
					State: v1.ChangedBlockTrackingDisabled,
				}
				reason := controller.verifyVMIEligibleForBackup(vmi)
				Expect(reason).ToNot(BeEmpty())
				Expect(reason).To(Equal(fmt.Sprintf(vmNoChangedBlockTrackingMsg, vmName)))
			})

			It("should succeed when VMI has eligible volumes and CBT enabled", func() {
				vmi := createVMI()
				reason := controller.verifyVMIEligibleForBackup(vmi)
				Expect(reason).To(BeEmpty())
			})
		})

		Context("hasVMIBackupStatus", func() {
			It("should return false when VMI is nil", func() {
				Expect(hasVMIBackupStatus(nil, backupName)).To(BeFalse())
			})

			It("should return false when ChangedBlockTracking is nil", func() {
				vmi := createVMI()
				vmi.Status.ChangedBlockTracking = nil
				Expect(hasVMIBackupStatus(vmi, backupName)).To(BeFalse())
			})

			It("should return false when BackupStatus is nil", func() {
				vmi := createVMI()
				Expect(hasVMIBackupStatus(vmi, backupName)).To(BeFalse())
			})

			It("should return false when BackupStatus belongs to a different backup", func() {
				vmi := createVMI()
				vmi.Status.ChangedBlockTracking.BackupStatus = &v1.VirtualMachineInstanceBackupStatus{
					BackupName: "other-backup",
				}
				Expect(hasVMIBackupStatus(vmi, backupName)).To(BeFalse())
			})

			It("should return true when BackupStatus matches the backup name", func() {
				vmi := createVMI()
				vmi.Status.ChangedBlockTracking.BackupStatus = &v1.VirtualMachineInstanceBackupStatus{
					BackupName: backupName,
				}
				Expect(hasVMIBackupStatus(vmi, backupName)).To(BeTrue())
			})
		})

		Context("sync during initialization", func() {
			It("should wait when VM doesn't exist", func() {
				backup := createBackup(backupName, vmName, pvcName, backupv1.PushMode)
				controller.backupInformer.GetStore().Add(backup)

				err := controller.sync(backup)
				Expect(err).ToNot(HaveOccurred())
				cond := meta.FindStatusCondition(backup.Status.Conditions, string(backupv1.ConditionInitializing))
				Expect(cond).ToNot(BeNil())
				Expect(cond.Message).To(Equal(fmt.Sprintf(vmNotFoundMsg, testNamespace, vmName)))
			})

			It("should wait when VMI doesn't exist", func() {
				backup := createBackup(backupName, vmName, pvcName, backupv1.PushMode)
				vm := createVM(vmName)
				controller.vmStore.Add(vm)
				controller.backupInformer.GetStore().Add(backup)

				err := controller.sync(backup)
				Expect(err).ToNot(HaveOccurred())
				cond := meta.FindStatusCondition(backup.Status.Conditions, string(backupv1.ConditionInitializing))
				Expect(cond).ToNot(BeNil())
				Expect(cond.Message).To(Equal(fmt.Sprintf(vmNotRunningMsg, vmName)))
			})

			It("should wait when VMI doesn't have CBT eligible volumes", func() {
				backup := createBackup(backupName, vmName, pvcName, backupv1.PushMode)
				vm := createVM(vmName)
				controller.vmStore.Add(vm)
				vmi := createVMI()
				vmi.Spec.Volumes = []v1.Volume{}
				controller.vmiStore.Add(vmi)
				controller.backupInformer.GetStore().Add(backup)

				err := controller.sync(backup)
				Expect(err).ToNot(HaveOccurred())
				cond := meta.FindStatusCondition(backup.Status.Conditions, string(backupv1.ConditionInitializing))
				Expect(cond).ToNot(BeNil())
				Expect(cond.Message).To(Equal(fmt.Sprintf(vmNoVolumesToBackupMsg, vmName)))
			})

			It("should wait when ChangedBlockTracking is not enabled", func() {
				backup := createBackup(backupName, vmName, pvcName, backupv1.PushMode)
				vm := createVM(vmName)
				controller.vmStore.Add(vm)
				vmi := createVMI()
				vmi.Status.ChangedBlockTracking = &v1.ChangedBlockTrackingStatus{
					State: v1.ChangedBlockTrackingDisabled,
				}
				controller.vmiStore.Add(vmi)
				controller.backupInformer.GetStore().Add(backup)

				err := controller.sync(backup)
				Expect(err).ToNot(HaveOccurred())
				cond := meta.FindStatusCondition(backup.Status.Conditions, string(backupv1.ConditionInitializing))
				Expect(cond).ToNot(BeNil())
				Expect(cond.Message).To(Equal(fmt.Sprintf(vmNoChangedBlockTrackingMsg, vmName)))
			})

			It("should wait when VMI is migrating and update initializing condition", func() {
				backup := createBackup(backupName, vmName, pvcName, backupv1.PushMode)
				vm := createVM(vmName)
				controller.vmStore.Add(vm)
				vmi := createVMI()
				now := metav1.Now()
				vmi.Status.MigrationState = &v1.VirtualMachineInstanceMigrationState{
					StartTimestamp: &now,
				}
				controller.vmiStore.Add(vmi)

				err := controller.sync(backup)
				Expect(err).ToNot(HaveOccurred())
				cond := meta.FindStatusCondition(backup.Status.Conditions, string(backupv1.ConditionInitializing))
				Expect(cond).ToNot(BeNil())
				Expect(cond.Status).To(Equal(metav1.ConditionTrue))
				Expect(cond.Message).To(ContainSubstring(fmt.Sprintf(vmMigrationInProgressMsg, vmName)))
			})

			It("should proceed when VMI migration has completed", func() {
				backup := createBackup(backupName, vmName, pvcName, backupv1.PushMode)
				backup.Finalizers = []string{vmBackupFinalizer}
				vm := createVM(vmName)
				controller.vmStore.Add(vm)
				vmi := createVMIWithPVCAttached()
				now := metav1.Now()
				vmi.Status.MigrationState = &v1.VirtualMachineInstanceMigrationState{
					StartTimestamp: &now,
					EndTimestamp:   &now,
				}
				controller.vmiStore.Add(vmi)
				pvc := createPVC(pvcName)
				controller.pvcStore.Add(pvc)
				controller.backupInformer.GetStore().Add(backup)

				vmiInterface.EXPECT().
					Patch(gomock.Any(), vmName, k8stypes.JSONPatchType, gomock.Any(), gomock.Any()).
					Return(vmi, nil)

				backupCalled := false
				vmiInterface.EXPECT().
					Backup(gomock.Any(), vmName, gomock.Any()).
					DoAndReturn(func(ctx context.Context, name string, options *backupv1.BackupOptions) error {
						backupCalled = true
						return nil
					})

				err := controller.sync(backup)
				Expect(err).ToNot(HaveOccurred())
				Expect(meta.IsStatusConditionTrue(backup.Status.Conditions, string(backupv1.ConditionProgressing))).To(BeTrue())
				Expect(backupCalled).To(BeTrue())
			})

		})
	})

	Context("addBackupFinalizer", func() {
		It("should add finalizer when not present", func() {
			backup := createBackup(backupName, vmName, pvcName, backupv1.PushMode)
			Expect(backup.Finalizers).To(BeEmpty())

			addBackup(backup)

			patched := false
			kubevirtClient.Fake.PrependReactor("patch", "virtualmachinebackups", func(action testing.Action) (handled bool, obj runtime.Object, err error) {
				patchAction := action.(testing.PatchAction)
				Expect(patchAction.GetPatchType()).To(Equal(k8stypes.JSONPatchType))
				patched = true

				updatedBackup := backup.DeepCopy()
				updatedBackup.Finalizers = []string{vmBackupFinalizer}
				return true, updatedBackup, nil
			})

			err := controller.addBackupFinalizer(backup)
			Expect(err).ToNot(HaveOccurred())
			Expect(patched).To(BeTrue())
		})

		It("should not re-add finalizer if already present", func() {
			backup := createBackup(backupName, vmName, pvcName, backupv1.PushMode)
			backup.Finalizers = []string{vmBackupFinalizer}

			kubevirtClient.Fake.PrependReactor("patch", "virtualmachinebackups", func(action testing.Action) (handled bool, obj runtime.Object, err error) {
				Fail("Should not patch when finalizer already exists")
				return true, nil, fmt.Errorf("unexpected patch call")
			})

			err := controller.addBackupFinalizer(backup)
			Expect(err).ToNot(HaveOccurred())
		})
	})

	It("should do nothing when backup already done", func() {
		backup := createBackup(backupName, vmName, pvcName, backupv1.PushMode)
		backup.Finalizers = []string{vmBackupFinalizer}
		backup.Status = &backupv1.VirtualMachineBackupStatus{
			Conditions: []metav1.Condition{
				newCondition(string(backupv1.ConditionProgressing), metav1.ConditionFalse, "Progressing", ""),
				newCondition(string(backupv1.ConditionComplete), metav1.ConditionTrue, "Completed", ""),
			},
		}

		vm := createVM(vmName)
		controller.vmStore.Add(vm)

		vmi := createVMI()
		controller.vmiStore.Add(vmi)

		pvc := createPVC(pvcName)
		controller.pvcStore.Add(pvc)

		err := controller.sync(backup)
		Expect(err).ToNot(HaveOccurred())
	})

	It("should retry start when VMI backup status is missing but VMI is eligible", func() {
		backup := createBackup(backupName, vmName, pvcName, backupv1.PushMode)
		backup.Finalizers = []string{vmBackupFinalizer}

		vm := createVM(vmName)
		controller.vmStore.Add(vm)

		vmi := createVMIWithPVCAttached()
		controller.vmiStore.Add(vmi)

		pvc := createPVC(pvcName)
		controller.pvcStore.Add(pvc)

		vmiInterface.EXPECT().
			Patch(gomock.Any(), vmName, k8stypes.JSONPatchType, gomock.Any(), gomock.Any()).
			Return(vmi, nil)

		vmiInterface.EXPECT().
			Backup(gomock.Any(), vmName, gomock.Any()).
			Return(nil)

		err := controller.sync(backup)
		Expect(err).ToNot(HaveOccurred())
		Expect(meta.IsStatusConditionTrue(backup.Status.Conditions, string(backupv1.ConditionProgressing))).To(BeTrue())
	})

	It("should recover from RPC failure and retry startBackup on next sync", func() {
		backup := createBackup(backupName, vmName, pvcName, backupv1.PushMode)
		backup.Finalizers = []string{vmBackupFinalizer}

		vm := createVM(vmName)
		controller.vmStore.Add(vm)

		vmi := createVMIWithPVCAttached()
		controller.vmiStore.Add(vmi)

		pvc := createPVC(pvcName)
		controller.pvcStore.Add(pvc)

		// First sync: RPC fails before updateSourceBackupInProgress
		vmiInterface.EXPECT().
			Backup(gomock.Any(), vmName, gomock.Any()).
			Return(fmt.Errorf("api error"))

		err := controller.sync(backup)
		Expect(err).To(HaveOccurred())
		Expect(err.Error()).To(ContainSubstring("failed to send Start backup command"))
		Expect(meta.IsStatusConditionTrue(backup.Status.Conditions, string(backupv1.ConditionProgressing))).To(BeFalse())

		// Second sync: no BackupStatus on VMI -> routes to reconcileStart -> startBackup retries
		vmiInterface.EXPECT().
			Backup(gomock.Any(), vmName, gomock.Any()).
			Return(nil)

		vmiInterface.EXPECT().
			Patch(gomock.Any(), vmName, k8stypes.JSONPatchType, gomock.Any(), gomock.Any()).
			Return(vmi, nil)

		err = controller.sync(backup)
		Expect(err).ToNot(HaveOccurred())
		Expect(meta.IsStatusConditionTrue(backup.Status.Conditions, string(backupv1.ConditionProgressing))).To(BeTrue())
	})

	It("should fail backup when VMI backup status is lost while progressing", func() {
		backup := createBackup(backupName, vmName, pvcName, backupv1.PushMode)
		backup.Finalizers = []string{vmBackupFinalizer}
		backup.Status = &backupv1.VirtualMachineBackupStatus{
			Conditions: []metav1.Condition{
				newCondition(string(backupv1.ConditionProgressing), metav1.ConditionTrue, "Progressing", backupInProgress),
			},
		}

		vm := createVM(vmName)
		controller.vmStore.Add(vm)
		vmi := createVMI()
		controller.vmiStore.Add(vmi)

		err := controller.sync(backup)
		Expect(err).ToNot(HaveOccurred())
		Expect(meta.IsStatusConditionTrue(backup.Status.Conditions, string(backupv1.ConditionFailed))).To(BeTrue())
		failureCond := meta.FindStatusCondition(backup.Status.Conditions, string(backupv1.ConditionFailed))
		Expect(failureCond.Message).To(ContainSubstring("VMI backup status was lost"))
	})

	Context("Backup deletion cleanup", func() {
		It("should populate includedVolumes early when backup in progress and volumes available", func() {
			backup := createBackup(backupName, vmName, pvcName, backupv1.PushMode)
			backup.Finalizers = []string{vmBackupFinalizer}
			backup.Status = &backupv1.VirtualMachineBackupStatus{
				Conditions: []metav1.Condition{
					newCondition(string(backupv1.ConditionProgressing), metav1.ConditionTrue, "Progressing", ""),
				},
			}

			vm := createVM(vmName)
			controller.vmStore.Add(vm)

			volumesInfo := []backupv1.BackupVolumeInfo{
				{VolumeName: "rootdisk", DiskTarget: "vda"},
				{VolumeName: "datadisk", DiskTarget: "vdb"},
			}
			vmi := createInitializedVMI()
			vmi.Status.ChangedBlockTracking.BackupStatus.Completed = false
			vmi.Status.ChangedBlockTracking.BackupStatus.Volumes = volumesInfo
			controller.vmiStore.Add(vmi)

			pvc := createPVC(pvcName)
			controller.pvcStore.Add(pvc)

			err := controller.sync(backup)
			Expect(err).ToNot(HaveOccurred())
			Expect(backup.Status.IncludedVolumes).To(HaveLen(2))
			Expect(backup.Status.IncludedVolumes[0].VolumeName).To(Equal("rootdisk"))
			Expect(backup.Status.IncludedVolumes[1].VolumeName).To(Equal("datadisk"))
		})

		It("should not update includedVolumes when already set in backup status", func() {
			existingVolumes := []backupv1.BackupVolumeInfo{
				{VolumeName: "rootdisk", DiskTarget: "vda"},
			}
			backup := createBackup(backupName, vmName, pvcName, backupv1.PushMode)
			backup.Finalizers = []string{vmBackupFinalizer}
			backup.Status = &backupv1.VirtualMachineBackupStatus{
				Conditions: []metav1.Condition{
					newCondition(string(backupv1.ConditionProgressing), metav1.ConditionTrue, "Progressing", ""),
				},
				IncludedVolumes: existingVolumes,
			}

			vm := createVM(vmName)
			controller.vmStore.Add(vm)

			vmi := createInitializedVMI()
			vmi.Status.ChangedBlockTracking.BackupStatus.Completed = false
			vmi.Status.ChangedBlockTracking.BackupStatus.Volumes = existingVolumes
			controller.vmiStore.Add(vmi)

			pvc := createPVC(pvcName)
			controller.pvcStore.Add(pvc)

			err := controller.sync(backup)
			Expect(err).ToNot(HaveOccurred())
		})

		It("should complete backup when VMI backup status is completed and VMI is cleaned up", func() {
			backup := createBackup(backupName, vmName, pvcName, backupv1.PushMode)
			backup.Finalizers = []string{vmBackupFinalizer}
			backup.Status = &backupv1.VirtualMachineBackupStatus{
				Conditions: []metav1.Condition{
					newCondition(string(backupv1.ConditionProgressing), metav1.ConditionTrue, "Progressing", ""),
				},
			}
			vm := createVM(vmName)
			controller.vmStore.Add(vm)
			vmi := createVMI()
			vmi.Status.ChangedBlockTracking.BackupStatus = &v1.VirtualMachineInstanceBackupStatus{
				BackupName:     backupName,
				Completed:      true,
				CheckpointName: pointer.P(checkpointName),
			}
			controller.vmiStore.Add(vmi)
			pvc := createPVC(pvcName)
			controller.pvcStore.Add(pvc)

			vmiInterface.EXPECT().
				Patch(gomock.Any(), vmName, k8stypes.JSONPatchType, gomock.Any(), gomock.Any()).
				Return(vmi, nil)

			err := controller.sync(backup)
			Expect(err).ToNot(HaveOccurred())
			Expect(meta.IsStatusConditionTrue(backup.Status.Conditions, string(backupv1.ConditionComplete))).To(BeTrue())
		})

		It("should complete backup with warning when BackupMsg is present", func() {
			backup := createBackup(backupName, vmName, pvcName, backupv1.PushMode)
			backup.Finalizers = []string{vmBackupFinalizer}
			backup.Status = &backupv1.VirtualMachineBackupStatus{
				Conditions: []metav1.Condition{
					newCondition(string(backupv1.ConditionProgressing), metav1.ConditionTrue, "Progressing", ""),
				},
			}
			vm := createVM(vmName)
			controller.vmStore.Add(vm)
			vmi := createVMI()
			vmi.Status.ChangedBlockTracking.BackupStatus = &v1.VirtualMachineInstanceBackupStatus{
				BackupName:     backupName,
				Completed:      true,
				CheckpointName: pointer.P(checkpointName),
				BackupMsg:      pointer.P("disk vdb was skipped"),
			}
			controller.vmiStore.Add(vmi)
			pvc := createPVC(pvcName)
			controller.pvcStore.Add(pvc)

			vmiInterface.EXPECT().
				Patch(gomock.Any(), vmName, k8stypes.JSONPatchType, gomock.Any(), gomock.Any()).
				Return(vmi, nil)

			err := controller.sync(backup)
			Expect(err).ToNot(HaveOccurred())
			Expect(meta.IsStatusConditionTrue(backup.Status.Conditions, string(backupv1.ConditionComplete))).To(BeTrue())
			completeCond := meta.FindStatusCondition(backup.Status.Conditions, string(backupv1.ConditionComplete))
			Expect(completeCond.Reason).To(Equal("CompletedWithWarning"))
			Eventually(recorder.Events).Should(Receive(ContainSubstring(backupCompletedWithWarningEvent)))
		})

		It("should fail backup when virt-handler reports failure (non-abort)", func() {
			backup := createBackup(backupName, vmName, pvcName, backupv1.PushMode)
			backup.Finalizers = []string{vmBackupFinalizer}
			backup.Status = &backupv1.VirtualMachineBackupStatus{
				Conditions: []metav1.Condition{
					newCondition(string(backupv1.ConditionProgressing), metav1.ConditionTrue, "Progressing", ""),
				},
			}
			vm := createVM(vmName)
			controller.vmStore.Add(vm)
			vmi := createVMI()
			vmi.Status.ChangedBlockTracking.BackupStatus = &v1.VirtualMachineInstanceBackupStatus{
				BackupName: backupName,
				Completed:  true,
				Failed:     true,
				BackupMsg:  pointer.P("disk error"),
			}
			controller.vmiStore.Add(vmi)
			pvc := createPVC(pvcName)
			controller.pvcStore.Add(pvc)

			vmiInterface.EXPECT().
				Patch(gomock.Any(), vmName, k8stypes.JSONPatchType, gomock.Any(), gomock.Any()).
				Return(vmi, nil)

			err := controller.sync(backup)
			Expect(err).ToNot(HaveOccurred())
			Expect(meta.IsStatusConditionTrue(backup.Status.Conditions, string(backupv1.ConditionFailed))).To(BeTrue())
			failureCond := meta.FindStatusCondition(backup.Status.Conditions, string(backupv1.ConditionFailed))
			Expect(failureCond.Message).To(ContainSubstring("disk error"))
			Expect(meta.IsStatusConditionTrue(backup.Status.Conditions, string(backupv1.ConditionAborting))).To(BeFalse())
		})

		It("should remove finalizer when a completed backup is being deleted", func() {
			backup := createBackup(backupName, vmName, pvcName, backupv1.PushMode)
			backup.Finalizers = []string{vmBackupFinalizer}
			backup.Status = &backupv1.VirtualMachineBackupStatus{
				Conditions: []metav1.Condition{
					newCondition(string(backupv1.ConditionProgressing), metav1.ConditionFalse, "Progressing", ""),
					newCondition(string(backupv1.ConditionComplete), metav1.ConditionTrue, "Completed", ""),
				},
			}
			backup.DeletionTimestamp = &metav1.Time{Time: metav1.Now().Time}

			finalizerPatched := false
			kubevirtClient.Fake.PrependReactor("patch", "virtualmachinebackups", func(action testing.Action) (handled bool, obj runtime.Object, err error) {
				finalizerPatched = true
				updatedBackup := backup.DeepCopy()
				updatedBackup.Finalizers = []string{}
				return true, updatedBackup, nil
			})

			err := controller.sync(backup)
			Expect(err).ToNot(HaveOccurred())
			Expect(finalizerPatched).To(BeTrue())
		})
	})

	Context("initialization failures", func() {
		It("should handle backup deletion during initialization when VMI is already gone", func() {
			backup := createBackup(backupName, vmName, pvcName, backupv1.PushMode)
			backup.DeletionTimestamp = &metav1.Time{Time: metav1.Now().Time}
			backup.Finalizers = []string{vmBackupFinalizer}

			patched := false
			kubevirtClient.Fake.PrependReactor("patch", "virtualmachinebackups", func(action testing.Action) (handled bool, obj runtime.Object, err error) {
				patchAction := action.(testing.PatchAction)
				if patchAction.GetName() == backupName {
					patched = true
				}
				return true, backup, nil
			})

			err := controller.sync(backup)
			Expect(err).ToNot(HaveOccurred())
			Expect(patched).To(BeTrue())
		})

		It("should handle backup deletion during initialization when the VMI exists", func() {
			backup := createBackup(backupName, vmName, pvcName, backupv1.PushMode)
			backup.DeletionTimestamp = &metav1.Time{Time: metav1.Now().Time}
			backup.Finalizers = []string{vmBackupFinalizer}

			vm := createVM(vmName)
			controller.vmStore.Add(vm)

			vmi := createVMI()
			controller.vmiStore.Add(vmi)

			err := controller.sync(backup)
			Expect(err).ToNot(HaveOccurred())
			Expect(meta.IsStatusConditionTrue(backup.Status.Conditions, string(backupv1.ConditionFailed))).To(BeTrue())
			failureCond := meta.FindStatusCondition(backup.Status.Conditions, string(backupv1.ConditionFailed))
			Expect(failureCond.Message).To(ContainSubstring("backup was deleted during initialization"))
		})

		It("should retry cleanup if it fails when backup is deleted during initialization", func() {
			backup := createBackup(backupName, vmName, pvcName, backupv1.PushMode)
			backup.DeletionTimestamp = &metav1.Time{Time: metav1.Now().Time}
			backup.Finalizers = []string{vmBackupFinalizer}

			vm := createVM(vmName)
			controller.vmStore.Add(vm)

			vmi := createVMI()
			vmi.Status.ChangedBlockTracking.BackupStatus = &v1.VirtualMachineInstanceBackupStatus{
				BackupName: "other-backup",
			}
			volumeName := backupTargetVolumeName(backupName)
			vmi.Spec.UtilityVolumes = []v1.UtilityVolume{
				{
					Name: volumeName,
					PersistentVolumeClaimVolumeSource: corev1.PersistentVolumeClaimVolumeSource{
						ClaimName: pvcName,
					},
					Type: pointer.P(v1.Backup),
				},
			}
			controller.vmiStore.Add(vmi)

			conflictErr := errors.NewApplyConflict([]metav1.StatusCause{}, "conflict error")
			vmiInterface.EXPECT().
				Patch(gomock.Any(), vmName, k8stypes.JSONPatchType, gomock.Any(), gomock.Any()).
				Return(nil, conflictErr)

			err := controller.sync(backup)
			Expect(err).To(HaveOccurred())
			Expect(err.Error()).To(ContainSubstring("conflict error"))
		})
	})

	Context("progressing failures", func() {
		It("should fail backup if VMI is deleted while backup is progressing", func() {
			backup := createBackup(backupName, vmName, pvcName, backupv1.PushMode)
			backup.Finalizers = []string{vmBackupFinalizer}

			vm := createVM(vmName)
			controller.vmStore.Add(vm)

			err := controller.sync(backup)
			Expect(err).ToNot(HaveOccurred())
			Expect(meta.IsStatusConditionTrue(backup.Status.Conditions, string(backupv1.ConditionFailed))).To(BeTrue())
			failureCond := meta.FindStatusCondition(backup.Status.Conditions, string(backupv1.ConditionFailed))
			Expect(failureCond.Message).To(Equal(fmt.Sprintf(backupFailed, "VMI was deleted during backup")))
		})

		It("should initiate abort if backup is deleted while progressing", func() {
			backupTracker := createBackupTracker(backupTrackerName, vmName, "new-checkpoint")
			controller.backupTrackerInformer.GetStore().Add(backupTracker)

			backup := createBackupWithTracker(backupName, vmName, pvcName)
			backup.DeletionTimestamp = &metav1.Time{Time: metav1.Now().Time}
			backup.Status = &backupv1.VirtualMachineBackupStatus{
				Conditions: []metav1.Condition{
					newCondition(string(backupv1.ConditionProgressing), metav1.ConditionTrue, "Progressing", ""),
				},
			}

			vm := createVM(vmName)
			controller.vmStore.Add(vm)
			vmi := createInitializedVMI()
			controller.vmiStore.Add(vmi)

			vmiInterface.EXPECT().
				Backup(gomock.Any(), vmName, gomock.Any()).
				DoAndReturn(func(ctx context.Context, name string, options *backupv1.BackupOptions) error {
					Expect(options.Cmd).To(Equal(backupv1.Abort))
					return nil
				})

			err := controller.sync(backup)
			Expect(err).ToNot(HaveOccurred())
			Expect(meta.IsStatusConditionTrue(backup.Status.Conditions, string(backupv1.ConditionAborting))).To(BeTrue())
		})

		It("should wait if backup is already marked as aborting", func() {
			backupTracker := createBackupTracker(backupTrackerName, vmName, "new-checkpoint")
			controller.backupTrackerInformer.GetStore().Add(backupTracker)

			backup := createBackupWithTracker(backupName, vmName, pvcName)
			backup.DeletionTimestamp = &metav1.Time{Time: metav1.Now().Time}
			backup.Status = &backupv1.VirtualMachineBackupStatus{
				Conditions: []metav1.Condition{
					newCondition(string(backupv1.ConditionProgressing), metav1.ConditionTrue, "Progressing", ""),
					newCondition(string(backupv1.ConditionAborting), metav1.ConditionTrue, "Aborting", backupAborting),
				},
			}

			vm := createVM(vmName)
			controller.vmStore.Add(vm)
			vmi := createInitializedVMI()
			controller.vmiStore.Add(vmi)

			err := controller.sync(backup)
			Expect(err).ToNot(HaveOccurred())
		})

		It("should finalize backup as failed when abort completes", func() {
			backupTracker := createBackupTracker(backupTrackerName, vmName, "new-checkpoint")
			controller.backupTrackerInformer.GetStore().Add(backupTracker)

			backup := createBackupWithTracker(backupName, vmName, pvcName)
			backup.DeletionTimestamp = &metav1.Time{Time: metav1.Now().Time}
			backup.Status = &backupv1.VirtualMachineBackupStatus{
				Conditions: []metav1.Condition{
					newCondition(string(backupv1.ConditionProgressing), metav1.ConditionTrue, "Progressing", ""),
					newCondition(string(backupv1.ConditionAborting), metav1.ConditionTrue, "Aborting", backupAborting),
				},
			}

			vm := createVM(vmName)
			controller.vmStore.Add(vm)

			vmiCanceled := createInitializedVMI()
			vmiCanceled.Spec.UtilityVolumes = nil
			vmiCanceled.Status.VolumeStatus = nil
			vmiCanceled.Status.ChangedBlockTracking.BackupStatus.Completed = true
			vmiCanceled.Status.ChangedBlockTracking.BackupStatus.Failed = true
			vmiCanceled.Status.ChangedBlockTracking.BackupStatus.BackupMsg = pointer.P("backup aborted")
			controller.vmiStore.Add(vmiCanceled)

			vmiInterface.EXPECT().
				Patch(gomock.Any(), vmName, k8stypes.JSONPatchType, gomock.Any(), gomock.Any()).
				Return(vmiCanceled, nil)

			kubevirtClient.Fake.PrependReactor("patch", "virtualmachinebackuptrackers", func(action testing.Action) (handled bool, obj runtime.Object, err error) {
				Fail("Backup was canceled and failed, should not update the tracker")
				return true, nil, nil
			})

			err := controller.sync(backup)
			Expect(err).ToNot(HaveOccurred())
			Expect(meta.IsStatusConditionTrue(backup.Status.Conditions, string(backupv1.ConditionFailed))).To(BeTrue())
			Expect(meta.IsStatusConditionTrue(backup.Status.Conditions, string(backupv1.ConditionAborting))).To(BeFalse())
			failureCond := meta.FindStatusCondition(backup.Status.Conditions, string(backupv1.ConditionFailed))
			Expect(failureCond.Message).To(Equal(fmt.Sprintf(backupFailed, "backup aborted")))
		})

		It("should fail backup when VMI stops running while progressing", func() {
			backup := createBackup(backupName, vmName, pvcName, backupv1.PushMode)
			backup.Status = &backupv1.VirtualMachineBackupStatus{
				Conditions: []metav1.Condition{
					newCondition(string(backupv1.ConditionProgressing), metav1.ConditionTrue, "Progressing", ""),
				},
			}

			vm := createVM(vmName)
			controller.vmStore.Add(vm)

			vmi := createInitializedVMI()
			vmi.Status.Phase = v1.Failed
			vmi.Spec.UtilityVolumes = nil
			vmi.Status.VolumeStatus = nil
			controller.vmiStore.Add(vmi)

			vmiInterface.EXPECT().
				Patch(gomock.Any(), vmName, k8stypes.JSONPatchType, gomock.Any(), gomock.Any()).
				Return(vmi, nil)

			err := controller.sync(backup)
			Expect(err).ToNot(HaveOccurred())
			Expect(meta.IsStatusConditionTrue(backup.Status.Conditions, string(backupv1.ConditionFailed))).To(BeTrue())
			failureCond := meta.FindStatusCondition(backup.Status.Conditions, string(backupv1.ConditionFailed))
			Expect(failureCond.Message).To(ContainSubstring("VMI is not in a running state"))
		})
	})

	It("should fail backup when VMI is gone and backup had finalizer", func() {
		backup := createBackup(backupName, vmName, pvcName, backupv1.PushMode)
		backup.Finalizers = []string{vmBackupFinalizer}

		err := controller.sync(backup)
		Expect(err).ToNot(HaveOccurred())
		Expect(meta.IsStatusConditionTrue(backup.Status.Conditions, string(backupv1.ConditionFailed))).To(BeTrue())
		failureCond := meta.FindStatusCondition(backup.Status.Conditions, string(backupv1.ConditionFailed))
		Expect(failureCond.Message).To(ContainSubstring("VMI was deleted during backup"))
	})

	Context("startBackup", func() {
		It("should return error if updateSourceBackupInProgress fails", func() {
			backup := createBackup(backupName, vmName, pvcName, backupv1.PushMode)
			backup.Finalizers = []string{vmBackupFinalizer}

			vm := createVM(vmName)
			controller.vmStore.Add(vm)
			vmi := createVMIWithPVCAttached()
			controller.vmiStore.Add(vmi)
			pvc := createPVC(pvcName)
			controller.pvcStore.Add(pvc)

			vmiInterface.EXPECT().
				Backup(gomock.Any(), vmName, gomock.Any()).
				Return(nil)

			vmiInterface.EXPECT().
				Patch(gomock.Any(), vmName, k8stypes.JSONPatchType, gomock.Any(), gomock.Any()).
				Return(nil, fmt.Errorf("patch failed"))

			err := controller.startBackup(backup, vmi, nil)
			Expect(err).To(HaveOccurred())
			Expect(err.Error()).To(ContainSubstring("failed to update source backup in progress"))
		})

		It("should return error if Start backup command fails", func() {
			backup := createBackup(backupName, vmName, pvcName, backupv1.PushMode)
			backup.Finalizers = []string{vmBackupFinalizer}

			vm := createVM(vmName)
			controller.vmStore.Add(vm)
			vmi := createInitializedVMI()
			controller.vmiStore.Add(vmi)
			pvc := createPVC(pvcName)
			controller.pvcStore.Add(pvc)

			vmiInterface.EXPECT().
				Backup(gomock.Any(), vmName, gomock.Any()).
				Return(fmt.Errorf("api error"))

			err := controller.startBackup(backup, vmi, nil)
			Expect(err).To(HaveOccurred())
			Expect(err.Error()).To(ContainSubstring("failed to send Start backup command"))
		})
	})

	Context("updateSourceBackupInProgress", func() {
		It("should fail when another backup is already in progress", func() {
			vmi := createVMI()
			vmi.Status.ChangedBlockTracking.BackupStatus = &v1.VirtualMachineInstanceBackupStatus{
				BackupName:     "other-backup",
				Completed:      false,
				CheckpointName: pointer.P("other-checkpoint"),
			}

			err := controller.updateSourceBackupInProgress(vmi, backupName, metav1.Now())
			Expect(err).To(HaveOccurred())
			Expect(err.Error()).To(ContainSubstring("another backup"))
			Expect(err.Error()).To(ContainSubstring("other-backup"))
			Expect(err.Error()).To(ContainSubstring("already in progress"))
		})

		It("should successfully patch VMI to add backup status", func() {
			vmi := createVMI()
			vmi.Status.ChangedBlockTracking.BackupStatus = nil

			patched := false
			vmiInterface.EXPECT().
				Patch(gomock.Any(), vmName, k8stypes.JSONPatchType, gomock.Any(), gomock.Any()).
				DoAndReturn(func(ctx context.Context, name string, patchType k8stypes.PatchType, patchBytes []byte, opts metav1.PatchOptions, subresources ...string) (*v1.VirtualMachineInstance, error) {
					patched = true
					Expect(string(patchBytes)).To(ContainSubstring("backupStatus"))
					Expect(string(patchBytes)).To(ContainSubstring(backupName))
					return vmi, nil
				})

			err := controller.updateSourceBackupInProgress(vmi, backupName, metav1.Now())
			Expect(err).ToNot(HaveOccurred())
			Expect(patched).To(BeTrue())
		})

		It("should return nil when same backup already in progress", func() {
			vmi := createVMI()
			vmi.Status.ChangedBlockTracking.BackupStatus = &v1.VirtualMachineInstanceBackupStatus{
				BackupName:     backupName,
				Completed:      false,
				CheckpointName: pointer.P(checkpointName),
			}

			vmiInterface.EXPECT().
				Patch(gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any()).
				Times(0)

			err := controller.updateSourceBackupInProgress(vmi, backupName, metav1.Now())
			Expect(err).ToNot(HaveOccurred())
		})
	})

	It("should attach PVC and return when PVC not yet attached", func() {
		backup := createBackup(backupName, vmName, pvcName, backupv1.PushMode)
		backup.Finalizers = []string{vmBackupFinalizer}

		vm := createVM(vmName)
		controller.vmStore.Add(vm)

		vmi := createVMI()
		controller.vmiStore.Add(vmi)

		pvc := createPVC(pvcName)
		controller.pvcStore.Add(pvc)

		patchCalled := false
		vmiInterface.EXPECT().
			Patch(gomock.Any(), vmName, k8stypes.JSONPatchType, gomock.Any(), gomock.Any()).
			DoAndReturn(func(ctx context.Context, name string, patchType k8stypes.PatchType, patchBytes []byte, opts metav1.PatchOptions, subresources ...string) (*v1.VirtualMachineInstance, error) {
				patchCalled = true
				Expect(string(patchBytes)).To(ContainSubstring("utilityVolumes"))
				Expect(string(patchBytes)).To(ContainSubstring(pvcName))
				return vmi, nil
			})

		err := controller.sync(backup)
		Expect(err).ToNot(HaveOccurred())
		Expect(patchCalled).To(BeTrue())
	})

	It("should successfully initiate backup and return backupInitiatedEvent with Full type", func() {
		backup := createBackup(backupName, vmName, pvcName, backupv1.PushMode)
		backup.Finalizers = []string{vmBackupFinalizer}

		vm := createVM(vmName)
		controller.vmStore.Add(vm)

		vmi := createVMIWithPVCAttached()
		controller.vmiStore.Add(vmi)

		pvc := createPVC(pvcName)
		controller.pvcStore.Add(pvc)

		vmiInterface.EXPECT().
			Patch(gomock.Any(), vmName, k8stypes.JSONPatchType, gomock.Any(), gomock.Any()).
			Return(vmi, nil)

		backupCalled := false
		vmiInterface.EXPECT().
			Backup(gomock.Any(), vmName, gomock.Any()).
			DoAndReturn(func(ctx context.Context, name string, options *backupv1.BackupOptions) error {
				backupCalled = true
				Expect(options.BackupName).To(Equal(backupName))
				Expect(options.Cmd).To(Equal(backupv1.Start))
				Expect(options.Mode).To(Equal(backupv1.PushMode))
				Expect(options.TargetPath).ToNot(BeNil())
				return nil
			})

		err := controller.sync(backup)
		Expect(err).ToNot(HaveOccurred())
		Expect(meta.IsStatusConditionTrue(backup.Status.Conditions, string(backupv1.ConditionProgressing))).To(BeTrue())
		Expect(backup.Status.Type).To(Equal(backupv1.Full))
		Expect(backupCalled).To(BeTrue())
	})

	It("should initiate full backup when backupTracker exists but has no LatestCheckpoint", func() {
		backupTracker := createBackupTracker(backupTrackerName, vmName, "")
		controller.backupTrackerInformer.GetStore().Add(backupTracker)

		backup := createBackupWithTracker(backupName, vmName, pvcName)
		backup.Finalizers = []string{vmBackupFinalizer}

		vm := createVM(vmName)
		controller.vmStore.Add(vm)

		vmi := createVMIWithPVCAttached()
		controller.vmiStore.Add(vmi)

		pvc := createPVC(pvcName)
		controller.pvcStore.Add(pvc)

		vmiInterface.EXPECT().
			Patch(gomock.Any(), vmName, k8stypes.JSONPatchType, gomock.Any(), gomock.Any()).
			Return(vmi, nil)

		backupCalled := false
		vmiInterface.EXPECT().
			Backup(gomock.Any(), vmName, gomock.Any()).
			DoAndReturn(func(ctx context.Context, name string, options *backupv1.BackupOptions) error {
				backupCalled = true
				Expect(options.BackupName).To(Equal(backupName))
				Expect(options.Cmd).To(Equal(backupv1.Start))
				Expect(options.Mode).To(Equal(backupv1.PushMode))
				Expect(options.TargetPath).ToNot(BeNil())
				Expect(options.Incremental).To(BeNil())
				return nil
			})

		err := controller.sync(backup)
		Expect(err).ToNot(HaveOccurred())
		Expect(meta.IsStatusConditionTrue(backup.Status.Conditions, string(backupv1.ConditionProgressing))).To(BeTrue())
		Expect(backup.Status.Type).To(Equal(backupv1.Full))
		Expect(backupCalled).To(BeTrue())
	})

	It("should initiate incremental backup when backupTracker has LatestCheckpoint", func() {
		backupTracker := createBackupTracker(backupTrackerName, vmName, checkpointName)
		controller.backupTrackerInformer.GetStore().Add(backupTracker)

		backup := createBackupWithTracker(backupName, vmName, pvcName)
		backup.Finalizers = []string{vmBackupFinalizer}

		vm := createVM(vmName)
		controller.vmStore.Add(vm)

		vmi := createVMIWithPVCAttached()
		controller.vmiStore.Add(vmi)

		pvc := createPVC(pvcName)
		controller.pvcStore.Add(pvc)

		vmiInterface.EXPECT().
			Patch(gomock.Any(), vmName, k8stypes.JSONPatchType, gomock.Any(), gomock.Any()).
			Return(vmi, nil)

		backupCalled := false
		vmiInterface.EXPECT().
			Backup(gomock.Any(), vmName, gomock.Any()).
			DoAndReturn(func(ctx context.Context, name string, options *backupv1.BackupOptions) error {
				backupCalled = true
				Expect(options.BackupName).To(Equal(backupName))
				Expect(options.Cmd).To(Equal(backupv1.Start))
				Expect(options.Mode).To(Equal(backupv1.PushMode))
				Expect(options.TargetPath).ToNot(BeNil())
				Expect(options.Incremental).ToNot(BeNil())
				Expect(*options.Incremental).To(Equal(checkpointName))
				return nil
			})

		err := controller.sync(backup)
		Expect(err).ToNot(HaveOccurred())
		Expect(meta.IsStatusConditionTrue(backup.Status.Conditions, string(backupv1.ConditionProgressing))).To(BeTrue())
		Expect(backup.Status.Type).To(Equal(backupv1.Incremental))
		Expect(backupCalled).To(BeTrue())
	})

	It("should initiate full backup with ForceFullBackup even with LatestCheckpoint", func() {
		backupTracker := createBackupTracker(backupTrackerName, vmName, checkpointName)
		controller.backupTrackerInformer.GetStore().Add(backupTracker)

		backup := createBackupWithTracker(backupName, vmName, pvcName)
		backup.Finalizers = []string{vmBackupFinalizer}
		backup.Spec.ForceFullBackup = true

		vm := createVM(vmName)
		controller.vmStore.Add(vm)

		vmi := createVMIWithPVCAttached()
		controller.vmiStore.Add(vmi)

		pvc := createPVC(pvcName)
		controller.pvcStore.Add(pvc)

		vmiInterface.EXPECT().
			Patch(gomock.Any(), vmName, k8stypes.JSONPatchType, gomock.Any(), gomock.Any()).
			Return(vmi, nil)

		backupCalled := false
		vmiInterface.EXPECT().
			Backup(gomock.Any(), vmName, gomock.Any()).
			DoAndReturn(func(ctx context.Context, name string, options *backupv1.BackupOptions) error {
				backupCalled = true
				Expect(options.BackupName).To(Equal(backupName))
				Expect(options.Cmd).To(Equal(backupv1.Start))
				Expect(options.Mode).To(Equal(backupv1.PushMode))
				Expect(options.TargetPath).ToNot(BeNil())
				Expect(options.Incremental).To(BeNil())
				return nil
			})

		err := controller.sync(backup)
		Expect(err).ToNot(HaveOccurred())
		Expect(meta.IsStatusConditionTrue(backup.Status.Conditions, string(backupv1.ConditionProgressing))).To(BeTrue())
		Expect(backup.Status.Type).To(Equal(backupv1.Full))
		Expect(backupCalled).To(BeTrue())
	})

	It("should complete cleanup and resolve completion when backup completed", func() {
		backup := createBackup(backupName, vmName, pvcName, backupv1.PushMode)
		backup.Finalizers = []string{vmBackupFinalizer}
		backup.Status = &backupv1.VirtualMachineBackupStatus{
			Conditions: []metav1.Condition{
				newCondition(string(backupv1.ConditionProgressing), metav1.ConditionTrue, "Progressing", ""),
			},
		}

		vm := createVM(vmName)
		controller.vmStore.Add(vm)

		vmi := createInitializedVMI()
		vmi.Status.ChangedBlockTracking.BackupStatus.Completed = true
		controller.vmiStore.Add(vmi)

		pvc := createPVC(pvcName)
		controller.pvcStore.Add(pvc)

		vmiInterface.EXPECT().
			Patch(gomock.Any(), vmName, k8stypes.JSONPatchType, gomock.Any(), gomock.Any()).
			Return(vmi, nil).
			Times(2)

		err := controller.sync(backup)
		Expect(err).ToNot(HaveOccurred())
		Expect(meta.IsStatusConditionTrue(backup.Status.Conditions, string(backupv1.ConditionComplete))).To(BeTrue())
	})

	It("should remove backup status from VMI and return completed event when already detached", func() {
		backup := createBackup(backupName, vmName, pvcName, backupv1.PushMode)
		backup.Finalizers = []string{vmBackupFinalizer}
		backup.Status = &backupv1.VirtualMachineBackupStatus{
			Conditions: []metav1.Condition{
				newCondition(string(backupv1.ConditionProgressing), metav1.ConditionTrue, "Progressing", ""),
			},
		}

		vm := createVM(vmName)
		controller.vmStore.Add(vm)

		volumesInfo := []backupv1.BackupVolumeInfo{
			{VolumeName: "rootdisk", DiskTarget: "vda"},
			{VolumeName: "datadisk", DiskTarget: "vdb"},
		}
		vmi := createVMI()
		vmi.Status.ChangedBlockTracking.BackupStatus = &v1.VirtualMachineInstanceBackupStatus{
			BackupName:     backupName,
			Completed:      true,
			CheckpointName: pointer.P(checkpointName),
			Volumes:        volumesInfo,
		}
		controller.vmiStore.Add(vmi)

		pvc := createPVC(pvcName)
		controller.pvcStore.Add(pvc)

		patchCalled := false
		vmiInterface.EXPECT().
			Patch(gomock.Any(), vmName, k8stypes.JSONPatchType, gomock.Any(), gomock.Any()).
			DoAndReturn(func(ctx context.Context, name string, patchType k8stypes.PatchType, patchBytes []byte, opts metav1.PatchOptions, subresources ...string) (*v1.VirtualMachineInstance, error) {
				patchCalled = true
				Expect(string(patchBytes)).To(ContainSubstring("backupStatus"))
				Expect(string(patchBytes)).To(ContainSubstring("remove"))
				return vmi, nil
			})

		err := controller.sync(backup)
		Expect(err).ToNot(HaveOccurred())
		Expect(meta.IsStatusConditionTrue(backup.Status.Conditions, string(backupv1.ConditionComplete))).To(BeTrue())
		Expect(patchCalled).To(BeTrue())
		Expect(backup.Status.IncludedVolumes).To(HaveLen(2))
		Expect(backup.Status.IncludedVolumes[0].VolumeName).To(Equal("rootdisk"))
		Expect(backup.Status.IncludedVolumes[1].VolumeName).To(Equal("datadisk"))
		Expect(backup.Status.CheckpointName).To(BeNil())
	})

	DescribeTable("should update backupTracker with checkpoint and volumes info when backup completes",
		func(existingCheckpoint string, expectedOp string) {
			backupTracker := createBackupTracker(backupTrackerName, vmName, existingCheckpoint)
			controller.backupTrackerInformer.GetStore().Add(backupTracker)

			backup := createBackupWithTracker(backupName, vmName, pvcName)
			backup.Finalizers = []string{vmBackupFinalizer}
			backup.Status = &backupv1.VirtualMachineBackupStatus{
				Conditions: []metav1.Condition{
					newCondition(string(backupv1.ConditionProgressing), metav1.ConditionTrue, "Progressing", ""),
				},
			}

			addBackup(backup)

			vm := createVM(vmName)
			controller.vmStore.Add(vm)

			volumesInfo := []backupv1.BackupVolumeInfo{
				{VolumeName: "rootdisk", DiskTarget: "vda"},
				{VolumeName: "datadisk", DiskTarget: "vdb"},
			}
			vmi := createVMI()
			vmi.Status.ChangedBlockTracking.BackupStatus = &v1.VirtualMachineInstanceBackupStatus{
				BackupName:     backupName,
				Completed:      true,
				CheckpointName: pointer.P(checkpointName),
				Volumes:        volumesInfo,
			}
			controller.vmiStore.Add(vmi)

			pvc := createPVC(pvcName)
			controller.pvcStore.Add(pvc)

			vmiInterface.EXPECT().
				Patch(gomock.Any(), vmName, k8stypes.JSONPatchType, gomock.Any(), gomock.Any()).
				Return(vmi, nil)

			trackerPatched := false
			kubevirtClient.Fake.PrependReactor("patch", "virtualmachinebackuptrackers", func(action testing.Action) (handled bool, obj runtime.Object, err error) {
				patchAction := action.(testing.PatchAction)
				Expect(patchAction.GetName()).To(Equal(backupTrackerName))
				Expect(patchAction.GetSubresource()).To(Equal("status"))

				patchBytes := patchAction.GetPatch()
				trackerPatched = true
				Expect(string(patchBytes)).To(ContainSubstring(expectedOp))
				Expect(string(patchBytes)).To(ContainSubstring("latestCheckpoint"))
				Expect(string(patchBytes)).To(ContainSubstring(checkpointName))
				Expect(string(patchBytes)).To(ContainSubstring("volumes"))
				Expect(string(patchBytes)).To(ContainSubstring("rootdisk"))
				Expect(string(patchBytes)).To(ContainSubstring("vda"))
				Expect(string(patchBytes)).To(ContainSubstring("datadisk"))
				Expect(string(patchBytes)).To(ContainSubstring("vdb"))

				updatedTracker := backupTracker.DeepCopy()
				updatedTracker.Status = &backupv1.VirtualMachineBackupTrackerStatus{
					LatestCheckpoint: &backupv1.BackupCheckpoint{
						Name:         checkpointName,
						CreationTime: &metav1.Time{Time: metav1.Now().Time},
						Volumes:      volumesInfo,
					},
				}
				return true, updatedTracker, nil
			})

			virtClient.EXPECT().VirtualMachineBackupTracker(testNamespace).
				Return(kubevirtClient.BackupV1alpha1().VirtualMachineBackupTrackers(testNamespace))

			err := controller.sync(backup)
			Expect(err).ToNot(HaveOccurred())
			Expect(meta.IsStatusConditionTrue(backup.Status.Conditions, string(backupv1.ConditionComplete))).To(BeTrue())
			Expect(trackerPatched).To(BeTrue())
			Expect(backup.Status.IncludedVolumes).To(HaveLen(2))
			Expect(backup.Status.IncludedVolumes[0].VolumeName).To(Equal("rootdisk"))
			Expect(backup.Status.IncludedVolumes[0].DiskTarget).To(Equal("vda"))
			Expect(backup.Status.IncludedVolumes[1].VolumeName).To(Equal("datadisk"))
			Expect(backup.Status.IncludedVolumes[1].DiskTarget).To(Equal("vdb"))
		},
		Entry("when tracker has no previous checkpoint", "", "\"op\":\"add\""),
		Entry("when tracker already has a checkpoint", "old-checkpoint", "\"op\":\"replace\""),
	)

	It("should update backupTracker even when cleanup returns early", func() {
		backupTracker := createBackupTracker(backupTrackerName, vmName, "")
		controller.backupTrackerInformer.GetStore().Add(backupTracker)

		backup := createBackupWithTracker(backupName, vmName, pvcName)
		backup.Finalizers = []string{vmBackupFinalizer}
		backup.Status = &backupv1.VirtualMachineBackupStatus{
			Conditions: []metav1.Condition{
				newCondition(string(backupv1.ConditionProgressing), metav1.ConditionTrue, "Progressing", ""),
			},
		}

		addBackup(backup)

		vm := createVM(vmName)
		controller.vmStore.Add(vm)

		vmi := createInitializedVMI()
		vmi.Status.ChangedBlockTracking.BackupStatus.Completed = true
		controller.vmiStore.Add(vmi)

		pvc := createPVC(pvcName)
		controller.pvcStore.Add(pvc)

		trackerPatched := false
		kubevirtClient.Fake.PrependReactor("patch", "virtualmachinebackuptrackers", func(action testing.Action) (handled bool, obj runtime.Object, err error) {
			patchAction := action.(testing.PatchAction)
			Expect(patchAction.GetName()).To(Equal(backupTrackerName))
			trackerPatched = true
			return true, backupTracker, nil
		})

		virtClient.EXPECT().VirtualMachineBackupTracker(testNamespace).
			Return(kubevirtClient.BackupV1alpha1().VirtualMachineBackupTrackers(testNamespace))

		vmiInterface.EXPECT().
			Patch(gomock.Any(), vmName, k8stypes.JSONPatchType, gomock.Any(), gomock.Any()).
			Return(vmi, nil).
			Times(2)

		err := controller.sync(backup)
		Expect(err).ToNot(HaveOccurred())
		Expect(trackerPatched).To(BeTrue())
		Expect(meta.IsStatusConditionTrue(backup.Status.Conditions, string(backupv1.ConditionComplete))).To(BeTrue())
	})

})
