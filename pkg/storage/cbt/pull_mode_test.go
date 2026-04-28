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
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"go.uber.org/mock/gomock"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/tools/record"

	backupv1 "kubevirt.io/api/backup/v1alpha1"
	v1 "kubevirt.io/api/core/v1"
	exportv1 "kubevirt.io/api/export/v1"
	"kubevirt.io/client-go/kubecli"
	kubevirtfake "kubevirt.io/client-go/kubevirt/fake"

	"kubevirt.io/kubevirt/pkg/pointer"
	"kubevirt.io/kubevirt/pkg/testutils"
)

var _ = Describe("Pull Mode", func() {
	var (
		ctrl           *gomock.Controller
		virtClient     *kubecli.MockKubevirtClient
		vmiInterface   *kubecli.MockVirtualMachineInstanceInterface
		backupCtrl     *VMBackupController
		kubevirtClient *kubevirtfake.Clientset
		recorder       *record.FakeRecorder
	)

	createPullBackup := func(name string, createdAgo time.Duration) *backupv1.VirtualMachineBackup {
		return &backupv1.VirtualMachineBackup{
			ObjectMeta: metav1.ObjectMeta{
				Name:              name,
				Namespace:         testNamespace,
				UID:               backupUID,
				CreationTimestamp: metav1.Time{Time: time.Now().Add(-createdAgo)},
			},
			Spec: backupv1.VirtualMachineBackupSpec{
				Mode: pointer.P(backupv1.PullMode),
			},
			Status: &backupv1.VirtualMachineBackupStatus{},
		}
	}

	createVMExport := func(name string, backup *backupv1.VirtualMachineBackup) *exportv1.VirtualMachineExport {
		return &exportv1.VirtualMachineExport{
			ObjectMeta: metav1.ObjectMeta{
				Name:      name,
				Namespace: testNamespace,
				OwnerReferences: []metav1.OwnerReference{
					*metav1.NewControllerRef(backup, backupv1.SchemeGroupVersion.WithKind(backupv1.VirtualMachineBackupGroupVersionKind.Kind)),
				},
			},
		}
	}

	BeforeEach(func() {
		ctrl = gomock.NewController(GinkgoT())
		virtClient = kubecli.NewMockKubevirtClient(ctrl)
		vmiInterface = kubecli.NewMockVirtualMachineInstanceInterface(ctrl)

		vmExportInformer, _ := testutils.NewFakeInformerFor(&exportv1.VirtualMachineExport{})

		recorder = record.NewFakeRecorder(100)

		backupCtrl = &VMBackupController{
			client:        virtClient,
			vmExportStore: vmExportInformer.GetStore(),
			recorder:      recorder,
		}

		kubevirtClient = kubevirtfake.NewSimpleClientset()
		virtClient.EXPECT().VirtualMachineExport(testNamespace).
			Return(kubevirtClient.ExportV1().VirtualMachineExports(testNamespace)).AnyTimes()
		virtClient.EXPECT().VirtualMachineInstance(testNamespace).Return(vmiInterface).AnyTimes()
	})

	AfterEach(func() {
		ctrl.Finish()
	})

	Context("mode predicates", func() {
		It("should identify push mode when mode is nil", func() {
			backup := &backupv1.VirtualMachineBackup{}
			Expect(isPushMode(backup)).To(BeTrue())
			Expect(isPullMode(backup)).To(BeFalse())
		})

		It("should identify push mode", func() {
			backup := &backupv1.VirtualMachineBackup{
				Spec: backupv1.VirtualMachineBackupSpec{Mode: pointer.P(backupv1.PushMode)},
			}
			Expect(isPushMode(backup)).To(BeTrue())
			Expect(isPullMode(backup)).To(BeFalse())
		})

		It("should identify pull mode", func() {
			backup := &backupv1.VirtualMachineBackup{
				Spec: backupv1.VirtualMachineBackupSpec{Mode: pointer.P(backupv1.PullMode)},
			}
			Expect(isPushMode(backup)).To(BeFalse())
			Expect(isPullMode(backup)).To(BeTrue())
		})
	})

	Context("TTL management", func() {
		It("should return default TTL when none specified", func() {
			backup := createPullBackup("test", 0)
			ttl := getPullBackupTTL(backup)
			Expect(ttl.Duration).To(Equal(defaultPullModeDurationTTL))
		})

		It("should return custom TTL when specified", func() {
			backup := createPullBackup("test", 0)
			backup.Spec.TTLDuration = &metav1.Duration{Duration: 30 * time.Minute}
			ttl := getPullBackupTTL(backup)
			Expect(ttl.Duration).To(Equal(30 * time.Minute))
		})

		It("should not be expired for a fresh backup", func() {
			backup := createPullBackup("test", 0)
			Expect(isPullBackupTTLExpired(backup)).To(BeFalse())
		})

		It("should be expired when TTL is exceeded", func() {
			backup := createPullBackup("test", 3*time.Hour)
			Expect(isPullBackupTTLExpired(backup)).To(BeTrue())
		})

		It("should calculate remaining TTL correctly", func() {
			backup := createPullBackup("test", 30*time.Minute)
			remaining := getPullBackupRemainingTTL(backup)
			Expect(remaining.Duration).To(BeNumerically("~", 90*time.Minute, 5*time.Second))
		})

		It("should return zero remaining TTL when expired", func() {
			backup := createPullBackup("test", 3*time.Hour)
			remaining := getPullBackupRemainingTTL(backup)
			Expect(remaining.Duration).To(Equal(time.Duration(0)))
		})
	})

	Context("getOrCreateBackupExport", func() {
		It("should return existing export owned by backup", func() {
			backup := createPullBackup(backupName, 0)
			vmExport := createVMExport(backupName, backup)
			backupCtrl.vmExportStore.Add(vmExport)

			result, err := backupCtrl.getOrCreateBackupExport(backup)
			Expect(err).ToNot(HaveOccurred())
			Expect(result).ToNot(BeNil())
			Expect(result.Name).To(Equal(backupName))
		})

		It("should error when export exists with different owner", func() {
			backup := createPullBackup(backupName, 0)
			otherBackup := createPullBackup("other-backup", 0)
			otherBackup.UID = "other-uid"
			vmExport := createVMExport(backupName, otherBackup)
			backupCtrl.vmExportStore.Add(vmExport)

			_, err := backupCtrl.getOrCreateBackupExport(backup)
			Expect(err).To(HaveOccurred())
			Expect(err.Error()).To(ContainSubstring("not owned by"))
		})

		It("should create export when it doesn't exist", func() {
			backup := createPullBackup(backupName, 0)
			backup.Spec.TokenSecretRef = "test-token"

			result, err := backupCtrl.getOrCreateBackupExport(backup)
			Expect(err).ToNot(HaveOccurred())
			Expect(result).To(BeNil())

			created, err := kubevirtClient.ExportV1().VirtualMachineExports(testNamespace).Get(
				context.Background(), backupName, metav1.GetOptions{})
			Expect(err).ToNot(HaveOccurred())
			Expect(created.Name).To(Equal(backupName))
		})
	})

	Context("cleanupBackupExport", func() {
		It("should delete existing export", func() {
			backup := createPullBackup(backupName, 0)
			vmExport := createVMExport(backupName, backup)
			backupCtrl.vmExportStore.Add(vmExport)

			_, err := kubevirtClient.ExportV1().VirtualMachineExports(testNamespace).Create(
				context.Background(), vmExport, metav1.CreateOptions{})
			Expect(err).ToNot(HaveOccurred())

			err = backupCtrl.cleanupBackupExport(backup)
			Expect(err).ToNot(HaveOccurred())
		})

		It("should succeed when export doesn't exist", func() {
			backup := createPullBackup(backupName, 0)
			err := backupCtrl.cleanupBackupExport(backup)
			Expect(err).ToNot(HaveOccurred())
		})
	})

	Context("handlePullModeTTLExpiry", func() {
		It("should initiate abort when VMI has active backup status", func() {
			backup := createPullBackup(backupName, 3*time.Hour)
			vmi := &v1.VirtualMachineInstance{
				ObjectMeta: metav1.ObjectMeta{Name: vmName, Namespace: testNamespace},
				Status: v1.VirtualMachineInstanceStatus{
					ChangedBlockTracking: &v1.ChangedBlockTrackingStatus{
						BackupStatus: &v1.VirtualMachineInstanceBackupStatus{
							BackupName: backupName,
							Completed:  false,
						},
					},
				},
			}

			vmiInterface.EXPECT().
				Backup(gomock.Any(), vmName, gomock.Any()).
				DoAndReturn(func(ctx context.Context, name string, options *backupv1.BackupOptions) error {
					Expect(options.Cmd).To(Equal(backupv1.Abort))
					return nil
				})

			err := backupCtrl.handlePullModeTTLExpiry(backup, vmi)
			Expect(err).ToNot(HaveOccurred())
		})

		It("should do nothing when VMI has no backup status", func() {
			backup := createPullBackup(backupName, 3*time.Hour)
			vmi := &v1.VirtualMachineInstance{
				ObjectMeta: metav1.ObjectMeta{Name: vmName, Namespace: testNamespace},
				Status:     v1.VirtualMachineInstanceStatus{},
			}

			err := backupCtrl.handlePullModeTTLExpiry(backup, vmi)
			Expect(err).ToNot(HaveOccurred())
		})

		It("should do nothing when backup is already completed on VMI", func() {
			backup := createPullBackup(backupName, 3*time.Hour)
			vmi := &v1.VirtualMachineInstance{
				ObjectMeta: metav1.ObjectMeta{Name: vmName, Namespace: testNamespace},
				Status: v1.VirtualMachineInstanceStatus{
					ChangedBlockTracking: &v1.ChangedBlockTrackingStatus{
						BackupStatus: &v1.VirtualMachineInstanceBackupStatus{
							BackupName: backupName,
							Completed:  true,
						},
					},
				},
			}

			err := backupCtrl.handlePullModeTTLExpiry(backup, vmi)
			Expect(err).ToNot(HaveOccurred())
		})
	})

	Context("populateExportLinks", func() {
		It("should return nil when backup has no included volumes", func() {
			backup := createPullBackup(backupName, 0)
			vmExport := createVMExport(backupName, backup)
			vmExport.Status = &exportv1.VirtualMachineExportStatus{
				Phase: exportv1.Ready,
			}

			err := backupCtrl.populateExportLinks(backup, vmExport)
			Expect(err).ToNot(HaveOccurred())
		})

		It("should error when export has no backup links", func() {
			backup := createPullBackup(backupName, 0)
			backup.Status.IncludedVolumes = []backupv1.BackupVolumeInfo{
				{VolumeName: "rootdisk"},
			}
			vmExport := createVMExport(backupName, backup)
			vmExport.Status = &exportv1.VirtualMachineExportStatus{
				Phase: exportv1.Ready,
				Links: &exportv1.VirtualMachineExportLinks{},
			}

			err := backupCtrl.populateExportLinks(backup, vmExport)
			Expect(err).To(HaveOccurred())
			Expect(err.Error()).To(ContainSubstring("no backup links"))
		})

		It("should populate endpoints from export links", func() {
			backup := createPullBackup(backupName, 0)
			backup.Status.IncludedVolumes = []backupv1.BackupVolumeInfo{
				{VolumeName: "rootdisk"},
			}
			vmExport := createVMExport(backupName, backup)
			vmExport.Status = &exportv1.VirtualMachineExportStatus{
				Phase: exportv1.Ready,
				Links: &exportv1.VirtualMachineExportLinks{
					Internal: &exportv1.VirtualMachineExportLink{
						Cert: "test-cert",
						Backups: []exportv1.VirtualMachineExportBackup{
							{
								Name: "rootdisk",
								Endpoints: []exportv1.VirtualMachineExportBackupEndpoint{
									{Endpoint: exportv1.Data, Url: "https://data.example.com"},
									{Endpoint: exportv1.Map, Url: "https://map.example.com"},
								},
							},
						},
					},
				},
			}

			err := backupCtrl.populateExportLinks(backup, vmExport)
			Expect(err).ToNot(HaveOccurred())
			Expect(backup.Status.EndpointCert).To(Equal(pointer.P("test-cert")))
		})
	})

	Context("handlePullMode observation-driven dispatch", func() {
		It("should create export when it doesn't exist", func() {
			backup := createPullBackup(backupName, 0)
			backup.Spec.TokenSecretRef = "test-token"
			vmi := &v1.VirtualMachineInstance{
				ObjectMeta: metav1.ObjectMeta{Name: vmName, Namespace: testNamespace},
			}

			err := backupCtrl.handlePullMode(backup, vmi)
			Expect(err).ToNot(HaveOccurred())
		})

		It("should wait when export has no status", func() {
			backup := createPullBackup(backupName, 0)
			vmExport := createVMExport(backupName, backup)
			backupCtrl.vmExportStore.Add(vmExport)
			vmi := &v1.VirtualMachineInstance{
				ObjectMeta: metav1.ObjectMeta{Name: vmName, Namespace: testNamespace},
			}

			err := backupCtrl.handlePullMode(backup, vmi)
			Expect(err).ToNot(HaveOccurred())
		})

		It("should wait when export has no service name", func() {
			backup := createPullBackup(backupName, 0)
			vmExport := createVMExport(backupName, backup)
			vmExport.Status = &exportv1.VirtualMachineExportStatus{}
			backupCtrl.vmExportStore.Add(vmExport)
			vmi := &v1.VirtualMachineInstance{
				ObjectMeta: metav1.ObjectMeta{Name: vmName, Namespace: testNamespace},
			}

			err := backupCtrl.handlePullMode(backup, vmi)
			Expect(err).ToNot(HaveOccurred())
		})

		It("should recreate export when it disappeared", func() {
			backup := createPullBackup(backupName, 0)
			backup.Spec.TokenSecretRef = "test-token"
			backup.Status.EndpointCert = pointer.P("some-cert")
			vmi := &v1.VirtualMachineInstance{
				ObjectMeta: metav1.ObjectMeta{Name: vmName, Namespace: testNamespace},
			}

			err := backupCtrl.handlePullMode(backup, vmi)
			Expect(err).ToNot(HaveOccurred())

			created, err := kubevirtClient.ExportV1().VirtualMachineExports(testNamespace).Get(
				context.Background(), backupName, metav1.GetOptions{})
			Expect(err).ToNot(HaveOccurred())
			Expect(created).ToNot(BeNil())
		})
	})
})

var _ = Describe("Pull Mode - export condition predicates", func() {
	It("should return false when export not initialized", func() {
		backup := &backupv1.VirtualMachineBackup{
			Status: &backupv1.VirtualMachineBackupStatus{},
		}
		Expect(isBackupExportInitialized(backup)).To(BeFalse())
	})

	It("should return true when export initialized condition is set", func() {
		backup := &backupv1.VirtualMachineBackup{
			Status: &backupv1.VirtualMachineBackupStatus{
				Conditions: []metav1.Condition{
					newCondition(string(backupv1.ConditionExportInitiated), metav1.ConditionTrue, "Initiated", ""),
				},
			},
		}
		Expect(isBackupExportInitialized(backup)).To(BeTrue())
	})
})
