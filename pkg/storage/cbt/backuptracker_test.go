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

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"go.uber.org/mock/gomock"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/util/validation/field"
	"k8s.io/client-go/tools/cache"
	"k8s.io/client-go/tools/record"

	backupv1 "kubevirt.io/api/backup/v1alpha1"
	v1 "kubevirt.io/api/core/v1"
	"kubevirt.io/client-go/kubecli"
	kubevirtfake "kubevirt.io/client-go/kubevirt/fake"

	"kubevirt.io/kubevirt/pkg/controller"
	"kubevirt.io/kubevirt/pkg/libvmi"
	"kubevirt.io/kubevirt/pkg/pointer"
	"kubevirt.io/kubevirt/pkg/testutils"
)

var _ = Describe("VMBackupController", func() {
	var (
		mockCtrl    *gomock.Controller
		virtClient  *kubecli.MockKubevirtClient
		kubevirtCli *kubevirtfake.Clientset
	)

	BeforeEach(func() {
		mockCtrl = gomock.NewController(GinkgoT())
		kubevirtCli = kubevirtfake.NewSimpleClientset()
		virtClient = kubecli.NewMockKubevirtClient(mockCtrl)
	})

	Context("trackerNeedsCheckpointRedefinition", func() {
		DescribeTable("should correctly identify trackers needing redefinition",
			func(tracker *backupv1.VirtualMachineBackupTracker, expected bool) {
				Expect(trackerNeedsCheckpointRedefinition(tracker)).To(Equal(expected))
			},
			Entry("needs redefinition when all conditions met",
				&backupv1.VirtualMachineBackupTracker{
					Status: &backupv1.VirtualMachineBackupTrackerStatus{
						CheckpointRedefinitionRequired: pointer.P(true),
						Checkpoints: []backupv1.BackupCheckpoint{{
							Name: "checkpoint-1",
						}},
					},
				},
				true,
			),
			Entry("does not need redefinition when tracker is nil",
				nil,
				false,
			),
			Entry("does not need redefinition when status is nil",
				&backupv1.VirtualMachineBackupTracker{
					Status: nil,
				},
				false,
			),
			Entry("does not need redefinition when flag is nil",
				&backupv1.VirtualMachineBackupTracker{
					Status: &backupv1.VirtualMachineBackupTrackerStatus{
						CheckpointRedefinitionRequired: nil,
						Checkpoints: []backupv1.BackupCheckpoint{{
							Name: "checkpoint-1",
						}},
					},
				},
				false,
			),
			Entry("does not need redefinition when flag is false",
				&backupv1.VirtualMachineBackupTracker{
					Status: &backupv1.VirtualMachineBackupTrackerStatus{
						CheckpointRedefinitionRequired: pointer.P(false),
						Checkpoints: []backupv1.BackupCheckpoint{{
							Name: "checkpoint-1",
						}},
					},
				},
				false,
			),
			Entry("does not need redefinition when checkpoints is nil",
				&backupv1.VirtualMachineBackupTracker{
					Status: &backupv1.VirtualMachineBackupTrackerStatus{
						CheckpointRedefinitionRequired: pointer.P(true),
					},
				},
				false,
			),
			Entry("does not need redefinition when checkpoints is empty",
				&backupv1.VirtualMachineBackupTracker{
					Status: &backupv1.VirtualMachineBackupTrackerStatus{
						CheckpointRedefinitionRequired: pointer.P(true),
						Checkpoints:                    []backupv1.BackupCheckpoint{},
					},
				},
				false,
			),
		)
	})

	Context("handleTrackerDeletion", func() {
		var (
			ctrl           *VMBackupController
			backupInformer cache.SharedIndexInformer
		)

		BeforeEach(func() {
			backupInformer, _ = testutils.NewFakeInformerWithIndexersFor(
				&backupv1.VirtualMachineBackup{},
				controller.GetVirtualMachineBackupInformerIndexers(),
			)

			ctrl = &VMBackupController{
				client:         virtClient,
				backupInformer: backupInformer,
			}
		})

		It("should remove finalizer when no backups exist", func() {
			tracker := createTracker("tracker1", "test-vmi", true, false)
			tracker.DeletionTimestamp = new(metav1.Now())
			tracker.Finalizers = []string{backupv1.VirtualMachineBackupTrackerFinalizer}
			_, err := kubevirtCli.BackupV1alpha1().VirtualMachineBackupTrackers(testNamespace).Create(
				context.Background(), tracker, metav1.CreateOptions{})
			Expect(err).ToNot(HaveOccurred())

			virtClient.EXPECT().VirtualMachineBackupTracker(testNamespace).
				Return(kubevirtCli.BackupV1alpha1().VirtualMachineBackupTrackers(testNamespace))

			err = ctrl.handleTrackerDeletion(tracker)
			Expect(err).ToNot(HaveOccurred())

			updated, err := kubevirtCli.BackupV1alpha1().VirtualMachineBackupTrackers(testNamespace).Get(
				context.Background(), "tracker1", metav1.GetOptions{})
			Expect(err).ToNot(HaveOccurred())
			Expect(updated.Finalizers).To(BeEmpty())
		})

		DescribeTable("should remove finalizer only when all backups are terminal",
			func(backup *backupv1.VirtualMachineBackup, shouldRemove bool) {
				tracker := createTracker("tracker1", "test-vmi", true, false)
				tracker.DeletionTimestamp = new(metav1.Now())
				tracker.Finalizers = []string{backupv1.VirtualMachineBackupTrackerFinalizer}
				_, err := kubevirtCli.BackupV1alpha1().VirtualMachineBackupTrackers(testNamespace).Create(
					context.Background(), tracker, metav1.CreateOptions{})
				Expect(err).ToNot(HaveOccurred())

				Expect(backupInformer.GetStore().Add(backup)).To(Succeed())

				if shouldRemove {
					virtClient.EXPECT().VirtualMachineBackupTracker(testNamespace).
						Return(kubevirtCli.BackupV1alpha1().VirtualMachineBackupTrackers(testNamespace))
				}

				err = ctrl.handleTrackerDeletion(tracker)
				if shouldRemove {
					Expect(err).ToNot(HaveOccurred())
				} else {
					Expect(err).To(HaveOccurred())
				}

				updated, err := kubevirtCli.BackupV1alpha1().VirtualMachineBackupTrackers(testNamespace).Get(
					context.Background(), "tracker1", metav1.GetOptions{})
				Expect(err).ToNot(HaveOccurred())

				if shouldRemove {
					Expect(updated.Finalizers).To(BeEmpty())
				} else {
					Expect(updated.Finalizers).To(ContainElement(backupv1.VirtualMachineBackupTrackerFinalizer))
				}
			},
			Entry("active backup blocks removal",
				&backupv1.VirtualMachineBackup{
					ObjectMeta: metav1.ObjectMeta{
						Name:      "active-backup",
						Namespace: testNamespace,
					},
					Spec: backupv1.VirtualMachineBackupSpec{
						Source: corev1.TypedLocalObjectReference{
							APIGroup: new(backupv1.SchemeGroupVersion.Group),
							Kind:     backupv1.VirtualMachineBackupTrackerGroupVersionKind.Kind,
							Name:     "tracker1",
						},
					},
				}, false),
			Entry("completed backup allows removal",
				&backupv1.VirtualMachineBackup{
					ObjectMeta: metav1.ObjectMeta{
						Name:      "completed-backup",
						Namespace: testNamespace,
					},
					Spec: backupv1.VirtualMachineBackupSpec{
						Source: corev1.TypedLocalObjectReference{
							APIGroup: new(backupv1.SchemeGroupVersion.Group),
							Kind:     backupv1.VirtualMachineBackupTrackerGroupVersionKind.Kind,
							Name:     "tracker1",
						},
					},
					Status: &backupv1.VirtualMachineBackupStatus{
						Conditions: []metav1.Condition{
							{
								Type:   string(backupv1.ConditionComplete),
								Status: metav1.ConditionTrue,
							},
						},
					},
				}, true),
		)
	})

	Context("executeTracker", func() {
		var (
			ctrl            *VMBackupController
			trackerInformer cache.SharedIndexInformer
			backupInformer  cache.SharedIndexInformer
			vmiInformer     cache.SharedIndexInformer
			recorder        *record.FakeRecorder
			vmiInterface    *kubecli.MockVirtualMachineInstanceInterface
		)

		BeforeEach(func() {
			trackerInformer, _ = testutils.NewFakeInformerWithIndexersFor(
				&backupv1.VirtualMachineBackupTracker{},
				controller.GetVirtualMachineBackupTrackerInformerIndexers(),
			)
			backupInformer, _ = testutils.NewFakeInformerWithIndexersFor(
				&backupv1.VirtualMachineBackup{},
				controller.GetVirtualMachineBackupInformerIndexers(),
			)
			vmiInformer, _ = testutils.NewFakeInformerFor(&v1.VirtualMachineInstance{})
			recorder = record.NewFakeRecorder(100)
			recorder.IncludeObject = true
			vmiInterface = kubecli.NewMockVirtualMachineInstanceInterface(mockCtrl)

			ctrl = &VMBackupController{
				client:                virtClient,
				backupTrackerInformer: trackerInformer,
				backupInformer:        backupInformer,
				vmiStore:              vmiInformer.GetStore(),
				recorder:              recorder,
			}
		})

		It("should return nil when tracker does not exist", func() {
			err := ctrl.executeTracker(testNamespace + "/nonexistent")
			Expect(err).ToNot(HaveOccurred())
		})

		It("should return nil when tracker no longer needs redefinition", func() {
			tracker := createTracker("tracker1", "test-vmi", false, false)
			Expect(trackerInformer.GetStore().Add(tracker)).To(Succeed())

			err := ctrl.executeTracker(testNamespace + "/tracker1")
			Expect(err).ToNot(HaveOccurred())
		})

		It("should return error when VMI not found", func() {
			tracker := createTracker("tracker1", "test-vmi", true, true)
			Expect(trackerInformer.GetStore().Add(tracker)).To(Succeed())
			// Don't add VMI to store

			err := ctrl.executeTracker(testNamespace + "/tracker1")
			Expect(err).To(HaveOccurred())
			Expect(err.Error()).To(ContainSubstring("not found"))
		})

		It("should call RedefineCheckpoint and clear flag on success", func() {
			tracker := createTracker("tracker1", "test-vmi", true, true)
			Expect(trackerInformer.GetStore().Add(tracker)).To(Succeed())
			_, err := kubevirtCli.BackupV1alpha1().VirtualMachineBackupTrackers(testNamespace).Create(
				context.Background(), tracker, metav1.CreateOptions{})
			Expect(err).ToNot(HaveOccurred())

			testVMI := libvmi.New(libvmi.WithNamespace(testNamespace), libvmi.WithName("test-vmi"))
			Expect(vmiInformer.GetStore().Add(testVMI)).To(Succeed())

			virtClient.EXPECT().VirtualMachineInstance(testNamespace).Return(vmiInterface)
			vmiInterface.EXPECT().RedefineCheckpoint(gomock.Any(), "test-vmi", gomock.Any(), gomock.Any()).Return(nil)
			virtClient.EXPECT().VirtualMachineBackupTracker(testNamespace).
				Return(kubevirtCli.BackupV1alpha1().VirtualMachineBackupTrackers(testNamespace))

			err = ctrl.executeTracker(testNamespace + "/tracker1")
			Expect(err).ToNot(HaveOccurred())

			// Verify flag was cleared
			updated, err := kubevirtCli.BackupV1alpha1().VirtualMachineBackupTrackers(testNamespace).Get(
				context.Background(), "tracker1", metav1.GetOptions{})
			Expect(err).ToNot(HaveOccurred())
			Expect(updated.Status.CheckpointRedefinitionRequired).To(BeNil())
			// Checkpoint should still exist
			Expect(updated.Status.Checkpoints).ToNot(BeEmpty())
		})

		It("should clear checkpoint on permanent error (HTTP 422)", func() {
			tracker := createTracker("tracker1", "test-vmi", true, true)
			Expect(trackerInformer.GetStore().Add(tracker)).To(Succeed())
			_, err := kubevirtCli.BackupV1alpha1().VirtualMachineBackupTrackers(testNamespace).Create(
				context.Background(), tracker, metav1.CreateOptions{})
			Expect(err).ToNot(HaveOccurred())

			testVMI := libvmi.New(libvmi.WithNamespace(testNamespace), libvmi.WithName("test-vmi"))
			Expect(vmiInformer.GetStore().Add(testVMI)).To(Succeed())

			invalidErr := apierrors.NewInvalid(
				schema.GroupKind{Group: "kubevirt.io", Kind: "VirtualMachineInstance"},
				"test-vmi",
				field.ErrorList{field.Invalid(field.NewPath("checkpoint"), "checkpoint-1", "bitmap invalid")},
			)

			virtClient.EXPECT().VirtualMachineInstance(testNamespace).Return(vmiInterface)
			vmiInterface.EXPECT().RedefineCheckpoint(gomock.Any(), "test-vmi", gomock.Any(), gomock.Any()).Return(invalidErr)
			vmiInterface.EXPECT().DeleteCheckpoint(gomock.Any(), "test-vmi", "checkpoint-1").Return(nil)
			virtClient.EXPECT().VirtualMachineBackupTracker(testNamespace).
				Return(kubevirtCli.BackupV1alpha1().VirtualMachineBackupTrackers(testNamespace))

			err = ctrl.executeTracker(testNamespace + "/tracker1")
			Expect(err).ToNot(HaveOccurred())

			// Verify both checkpoint and flag were cleared
			updated, err := kubevirtCli.BackupV1alpha1().VirtualMachineBackupTrackers(testNamespace).Get(
				context.Background(), "tracker1", metav1.GetOptions{})
			Expect(err).ToNot(HaveOccurred())
			Expect(updated.Status.CheckpointRedefinitionRequired).To(BeNil())
			Expect(updated.Status.Checkpoints).To(BeEmpty())

			// Verify event was emitted
			Eventually(recorder.Events).Should(Receive(ContainSubstring("CheckpointRedefinitionFailed")))
		})

		It("should return error for requeue on transient error (HTTP 503)", func() {
			tracker := createTracker("tracker1", "test-vmi", true, true)
			Expect(trackerInformer.GetStore().Add(tracker)).To(Succeed())
			_, err := kubevirtCli.BackupV1alpha1().VirtualMachineBackupTrackers(testNamespace).Create(
				context.Background(), tracker, metav1.CreateOptions{})
			Expect(err).ToNot(HaveOccurred())

			testVMI := libvmi.New(libvmi.WithNamespace(testNamespace), libvmi.WithName("test-vmi"))
			Expect(vmiInformer.GetStore().Add(testVMI)).To(Succeed())

			transientErr := apierrors.NewServiceUnavailable("service temporarily unavailable")

			virtClient.EXPECT().VirtualMachineInstance(testNamespace).Return(vmiInterface)
			vmiInterface.EXPECT().RedefineCheckpoint(gomock.Any(), "test-vmi", gomock.Any(), gomock.Any()).Return(transientErr)

			err = ctrl.executeTracker(testNamespace + "/tracker1")
			Expect(err).To(HaveOccurred())
			Expect(apierrors.IsServiceUnavailable(err)).To(BeTrue())

			// Verify tracker was NOT modified
			updated, err := kubevirtCli.BackupV1alpha1().VirtualMachineBackupTrackers(testNamespace).Get(
				context.Background(), "tracker1", metav1.GetOptions{})
			Expect(err).ToNot(HaveOccurred())
			Expect(updated.Status.CheckpointRedefinitionRequired).ToNot(BeNil())
			Expect(*updated.Status.CheckpointRedefinitionRequired).To(BeTrue())
			Expect(updated.Status.Checkpoints).ToNot(BeEmpty())

			// Verify no event was emitted
			Consistently(recorder.Events).ShouldNot(Receive())
		})

		It("should redefine multi-checkpoint chain with correct parent names", func() {
			tracker := createTracker("tracker1", "test-vmi", false, false)
			tracker.Status = &backupv1.VirtualMachineBackupTrackerStatus{
				Checkpoints: []backupv1.BackupCheckpoint{
					{Name: "cp-1"},
					{Name: "cp-2"},
					{Name: "cp-3"},
				},
				CheckpointRedefinitionRequired: new(true),
			}
			Expect(trackerInformer.GetStore().Add(tracker)).To(Succeed())
			_, err := kubevirtCli.BackupV1alpha1().VirtualMachineBackupTrackers(testNamespace).Create(
				context.Background(), tracker, metav1.CreateOptions{})
			Expect(err).ToNot(HaveOccurred())

			testVMI := libvmi.New(libvmi.WithNamespace(testNamespace), libvmi.WithName("test-vmi"))
			Expect(vmiInformer.GetStore().Add(testVMI)).To(Succeed())

			virtClient.EXPECT().VirtualMachineInstance(testNamespace).Return(vmiInterface)
			gomock.InOrder(
				vmiInterface.EXPECT().RedefineCheckpoint(gomock.Any(), "test-vmi", gomock.Any(), "").Return(nil),
				vmiInterface.EXPECT().RedefineCheckpoint(gomock.Any(), "test-vmi", gomock.Any(), "cp-1").Return(nil),
				vmiInterface.EXPECT().RedefineCheckpoint(gomock.Any(), "test-vmi", gomock.Any(), "cp-2").Return(nil),
			)
			virtClient.EXPECT().VirtualMachineBackupTracker(testNamespace).
				Return(kubevirtCli.BackupV1alpha1().VirtualMachineBackupTrackers(testNamespace))

			err = ctrl.executeTracker(testNamespace + "/tracker1")
			Expect(err).ToNot(HaveOccurred())

			updated, err := kubevirtCli.BackupV1alpha1().VirtualMachineBackupTrackers(testNamespace).Get(
				context.Background(), "tracker1", metav1.GetOptions{})
			Expect(err).ToNot(HaveOccurred())
			Expect(updated.Status.CheckpointRedefinitionRequired).To(BeNil())
			Expect(updated.Status.Checkpoints).To(HaveLen(3))
		})

		It("should truncate chain and delete orphans when middle checkpoint is invalid", func() {
			tracker := createTracker("tracker1", "test-vmi", false, false)
			tracker.Status = &backupv1.VirtualMachineBackupTrackerStatus{
				Checkpoints: []backupv1.BackupCheckpoint{
					{Name: "cp-1"},
					{Name: "cp-2"},
					{Name: "cp-3"},
				},
				CheckpointRedefinitionRequired: new(true),
			}
			Expect(trackerInformer.GetStore().Add(tracker)).To(Succeed())
			_, err := kubevirtCli.BackupV1alpha1().VirtualMachineBackupTrackers(testNamespace).Create(
				context.Background(), tracker, metav1.CreateOptions{})
			Expect(err).ToNot(HaveOccurred())

			testVMI := libvmi.New(libvmi.WithNamespace(testNamespace), libvmi.WithName("test-vmi"))
			Expect(vmiInformer.GetStore().Add(testVMI)).To(Succeed())

			invalidErr := apierrors.NewInvalid(
				schema.GroupKind{Group: "kubevirt.io", Kind: "VirtualMachineInstance"},
				"test-vmi",
				field.ErrorList{field.Invalid(field.NewPath("checkpoint"), "cp-2", "bitmap invalid")},
			)

			virtClient.EXPECT().VirtualMachineInstance(testNamespace).Return(vmiInterface)
			gomock.InOrder(
				vmiInterface.EXPECT().RedefineCheckpoint(gomock.Any(), "test-vmi", gomock.Any(), "").Return(nil),
				vmiInterface.EXPECT().RedefineCheckpoint(gomock.Any(), "test-vmi", gomock.Any(), "cp-1").Return(invalidErr),
			)
			// Orphaned checkpoints deleted in reverse order (leaf-to-root)
			gomock.InOrder(
				vmiInterface.EXPECT().DeleteCheckpoint(gomock.Any(), "test-vmi", "cp-3").Return(nil),
				vmiInterface.EXPECT().DeleteCheckpoint(gomock.Any(), "test-vmi", "cp-2").Return(nil),
			)
			virtClient.EXPECT().VirtualMachineBackupTracker(testNamespace).
				Return(kubevirtCli.BackupV1alpha1().VirtualMachineBackupTrackers(testNamespace))

			err = ctrl.executeTracker(testNamespace + "/tracker1")
			Expect(err).ToNot(HaveOccurred())

			updated, err := kubevirtCli.BackupV1alpha1().VirtualMachineBackupTrackers(testNamespace).Get(
				context.Background(), "tracker1", metav1.GetOptions{})
			Expect(err).ToNot(HaveOccurred())
			Expect(updated.Status.CheckpointRedefinitionRequired).To(BeNil())
			Expect(updated.Status.Checkpoints).To(HaveLen(1))
			Expect(updated.Status.Checkpoints[0].Name).To(Equal("cp-1"))

			Eventually(recorder.Events).Should(Receive(ContainSubstring("CheckpointRedefinitionFailed")))
		})

		It("should clear all checkpoints when first checkpoint is invalid", func() {
			tracker := createTracker("tracker1", "test-vmi", false, false)
			tracker.Status = &backupv1.VirtualMachineBackupTrackerStatus{
				Checkpoints: []backupv1.BackupCheckpoint{
					{Name: "cp-1"},
					{Name: "cp-2"},
				},
				CheckpointRedefinitionRequired: new(true),
			}
			Expect(trackerInformer.GetStore().Add(tracker)).To(Succeed())
			_, err := kubevirtCli.BackupV1alpha1().VirtualMachineBackupTrackers(testNamespace).Create(
				context.Background(), tracker, metav1.CreateOptions{})
			Expect(err).ToNot(HaveOccurred())

			testVMI := libvmi.New(libvmi.WithNamespace(testNamespace), libvmi.WithName("test-vmi"))
			Expect(vmiInformer.GetStore().Add(testVMI)).To(Succeed())

			invalidErr := apierrors.NewInvalid(
				schema.GroupKind{Group: "kubevirt.io", Kind: "VirtualMachineInstance"},
				"test-vmi",
				field.ErrorList{field.Invalid(field.NewPath("checkpoint"), "cp-1", "bitmap invalid")},
			)

			virtClient.EXPECT().VirtualMachineInstance(testNamespace).Return(vmiInterface)
			vmiInterface.EXPECT().RedefineCheckpoint(gomock.Any(), "test-vmi", gomock.Any(), "").Return(invalidErr)
			// Orphaned checkpoints deleted in reverse order
			gomock.InOrder(
				vmiInterface.EXPECT().DeleteCheckpoint(gomock.Any(), "test-vmi", "cp-2").Return(nil),
				vmiInterface.EXPECT().DeleteCheckpoint(gomock.Any(), "test-vmi", "cp-1").Return(nil),
			)
			virtClient.EXPECT().VirtualMachineBackupTracker(testNamespace).
				Return(kubevirtCli.BackupV1alpha1().VirtualMachineBackupTrackers(testNamespace))

			err = ctrl.executeTracker(testNamespace + "/tracker1")
			Expect(err).ToNot(HaveOccurred())

			updated, err := kubevirtCli.BackupV1alpha1().VirtualMachineBackupTrackers(testNamespace).Get(
				context.Background(), "tracker1", metav1.GetOptions{})
			Expect(err).ToNot(HaveOccurred())
			Expect(updated.Status.CheckpointRedefinitionRequired).To(BeNil())
			Expect(updated.Status.Checkpoints).To(BeEmpty())

			Eventually(recorder.Events).Should(Receive(ContainSubstring("CheckpointRedefinitionFailed")))
		})

		It("should return transient error without truncation on non-invalid error", func() {
			tracker := createTracker("tracker1", "test-vmi", false, false)
			tracker.Status = &backupv1.VirtualMachineBackupTrackerStatus{
				Checkpoints: []backupv1.BackupCheckpoint{
					{Name: "cp-1"},
					{Name: "cp-2"},
					{Name: "cp-3"},
				},
				CheckpointRedefinitionRequired: new(true),
			}
			Expect(trackerInformer.GetStore().Add(tracker)).To(Succeed())
			_, err := kubevirtCli.BackupV1alpha1().VirtualMachineBackupTrackers(testNamespace).Create(
				context.Background(), tracker, metav1.CreateOptions{})
			Expect(err).ToNot(HaveOccurred())

			testVMI := libvmi.New(libvmi.WithNamespace(testNamespace), libvmi.WithName("test-vmi"))
			Expect(vmiInformer.GetStore().Add(testVMI)).To(Succeed())

			transientErr := apierrors.NewServiceUnavailable("service temporarily unavailable")

			virtClient.EXPECT().VirtualMachineInstance(testNamespace).Return(vmiInterface)
			vmiInterface.EXPECT().RedefineCheckpoint(gomock.Any(), "test-vmi", gomock.Any(), "").Return(transientErr)

			err = ctrl.executeTracker(testNamespace + "/tracker1")
			Expect(err).To(HaveOccurred())
			Expect(apierrors.IsServiceUnavailable(err)).To(BeTrue())

			// Verify tracker was NOT modified — all 3 checkpoints still present
			updated, err := kubevirtCli.BackupV1alpha1().VirtualMachineBackupTrackers(testNamespace).Get(
				context.Background(), "tracker1", metav1.GetOptions{})
			Expect(err).ToNot(HaveOccurred())
			Expect(updated.Status.CheckpointRedefinitionRequired).ToNot(BeNil())
			Expect(updated.Status.Checkpoints).To(HaveLen(3))

			Consistently(recorder.Events).ShouldNot(Receive())
		})
	})

	Context("updateBackupTracker", func() {
		var (
			ctrl    *VMBackupController
			tracker *backupv1.VirtualMachineBackupTracker
		)

		BeforeEach(func() {
			ctrl = &VMBackupController{
				client: virtClient,
			}
		})

		It("should return nil when tracker is nil", func() {
			backupStatus := &v1.VirtualMachineInstanceBackupStatus{
				CheckpointName: new("cp-1"),
			}
			err := ctrl.updateBackupTracker(testNamespace, nil, backupv1.Full, backupStatus)
			Expect(err).ToNot(HaveOccurred())
		})

		It("should set checkpoint when tracker has nil status", func() {
			tracker = createTracker("tracker1", "test-vmi", false, false)
			_, err := kubevirtCli.BackupV1alpha1().VirtualMachineBackupTrackers(testNamespace).Create(
				context.Background(), tracker, metav1.CreateOptions{})
			Expect(err).ToNot(HaveOccurred())

			virtClient.EXPECT().VirtualMachineBackupTracker(testNamespace).
				Return(kubevirtCli.BackupV1alpha1().VirtualMachineBackupTrackers(testNamespace))

			backupStatus := &v1.VirtualMachineInstanceBackupStatus{
				CheckpointName: new("cp-1"),
				Volumes: []v1.VirtualMachineInstanceBackupVolumeInfo{
					{VolumeName: "rootdisk"},
				},
			}
			err = ctrl.updateBackupTracker(testNamespace, tracker, backupv1.Full, backupStatus)
			Expect(err).ToNot(HaveOccurred())

			updated, err := kubevirtCli.BackupV1alpha1().VirtualMachineBackupTrackers(testNamespace).Get(
				context.Background(), "tracker1", metav1.GetOptions{})
			Expect(err).ToNot(HaveOccurred())
			Expect(updated.Status).ToNot(BeNil())
			Expect(updated.Status.Checkpoints).To(HaveLen(1))
			Expect(updated.Status.LatestCheckpoint).ToNot(BeNil())
			Expect(updated.Status.LatestCheckpoint.Name).To(Equal("cp-1"))
			Expect(updated.Status.LatestCheckpoint.Volumes).To(HaveLen(1))
			Expect(updated.Status.LatestCheckpoint.Volumes[0]).To(Equal("rootdisk"))
		})

		It("should preserve existing status fields when updating checkpoint", func() {
			tracker = createTracker("tracker1", "test-vmi", true, true)
			_, err := kubevirtCli.BackupV1alpha1().VirtualMachineBackupTrackers(testNamespace).Create(
				context.Background(), tracker, metav1.CreateOptions{})
			Expect(err).ToNot(HaveOccurred())

			virtClient.EXPECT().VirtualMachineBackupTracker(testNamespace).
				Return(kubevirtCli.BackupV1alpha1().VirtualMachineBackupTrackers(testNamespace))

			backupStatus := &v1.VirtualMachineInstanceBackupStatus{
				CheckpointName: new("cp-2"),
				Volumes: []v1.VirtualMachineInstanceBackupVolumeInfo{
					{VolumeName: "datadisk"},
				},
			}
			err = ctrl.updateBackupTracker(testNamespace, tracker, backupv1.Full, backupStatus)
			Expect(err).ToNot(HaveOccurred())

			updated, err := kubevirtCli.BackupV1alpha1().VirtualMachineBackupTrackers(testNamespace).Get(
				context.Background(), "tracker1", metav1.GetOptions{})
			Expect(err).ToNot(HaveOccurred())
			Expect(updated.Status.Checkpoints).To(HaveLen(2))
			Expect(updated.Status.Checkpoints[0].Name).To(Equal("checkpoint-1"))
			Expect(updated.Status.LatestCheckpoint.Name).To(Equal("cp-2"))
			Expect(updated.Status.CheckpointRedefinitionRequired).ToNot(BeNil())
			Expect(*updated.Status.CheckpointRedefinitionRequired).To(BeTrue())
		})

		It("should not prune in updateBackupTracker (pruning is handled by tracker queue)", func() {
			tracker = createTracker("tracker1", "test-vmi", true, false)
			tracker.Spec.RetainCheckpoints = new(int32(2))
			tracker.Status = &backupv1.VirtualMachineBackupTrackerStatus{
				Checkpoints: []backupv1.BackupCheckpoint{
					{Name: "cp-old"},
					{Name: "cp-current"},
				},
			}
			_, err := kubevirtCli.BackupV1alpha1().VirtualMachineBackupTrackers(testNamespace).Create(
				context.Background(), tracker, metav1.CreateOptions{})
			Expect(err).ToNot(HaveOccurred())

			virtClient.EXPECT().VirtualMachineBackupTracker(testNamespace).
				Return(kubevirtCli.BackupV1alpha1().VirtualMachineBackupTrackers(testNamespace))

			backupStatus := &v1.VirtualMachineInstanceBackupStatus{
				CheckpointName: new("cp-new"),
			}
			err = ctrl.updateBackupTracker(testNamespace, tracker, backupv1.Incremental, backupStatus)
			Expect(err).ToNot(HaveOccurred())

			updated, err := kubevirtCli.BackupV1alpha1().VirtualMachineBackupTrackers(testNamespace).Get(
				context.Background(), "tracker1", metav1.GetOptions{})
			Expect(err).ToNot(HaveOccurred())
			Expect(updated.Status.Checkpoints).To(HaveLen(3))
			Expect(updated.Status.Checkpoints[0].Name).To(Equal("cp-old"))
			Expect(updated.Status.Checkpoints[1].Name).To(Equal("cp-current"))
			Expect(updated.Status.Checkpoints[2].Name).To(Equal("cp-new"))
		})

		It("should not prune when retainCheckpoints is nil", func() {
			tracker = createTracker("tracker1", "test-vmi", true, false)
			_, err := kubevirtCli.BackupV1alpha1().VirtualMachineBackupTrackers(testNamespace).Create(
				context.Background(), tracker, metav1.CreateOptions{})
			Expect(err).ToNot(HaveOccurred())

			virtClient.EXPECT().VirtualMachineBackupTracker(testNamespace).
				Return(kubevirtCli.BackupV1alpha1().VirtualMachineBackupTrackers(testNamespace))

			backupStatus := &v1.VirtualMachineInstanceBackupStatus{
				CheckpointName: new("cp-2"),
			}
			err = ctrl.updateBackupTracker(testNamespace, tracker, backupv1.Full, backupStatus)
			Expect(err).ToNot(HaveOccurred())

			updated, err := kubevirtCli.BackupV1alpha1().VirtualMachineBackupTrackers(testNamespace).Get(
				context.Background(), "tracker1", metav1.GetOptions{})
			Expect(err).ToNot(HaveOccurred())
			Expect(updated.Status.Checkpoints).To(HaveLen(2))
		})

		It("should be idempotent when checkpoint already exists", func() {
			tracker = createTracker("tracker1", "test-vmi", true, false)
			_, err := kubevirtCli.BackupV1alpha1().VirtualMachineBackupTrackers(testNamespace).Create(
				context.Background(), tracker, metav1.CreateOptions{})
			Expect(err).ToNot(HaveOccurred())

			backupStatus := &v1.VirtualMachineInstanceBackupStatus{
				CheckpointName: new(tracker.Status.LatestCheckpoint.Name),
			}
			err = ctrl.updateBackupTracker(testNamespace, tracker, backupv1.Full, backupStatus)
			Expect(err).ToNot(HaveOccurred())

			updated, err := kubevirtCli.BackupV1alpha1().VirtualMachineBackupTrackers(testNamespace).Get(
				context.Background(), "tracker1", metav1.GetOptions{})
			Expect(err).ToNot(HaveOccurred())
			Expect(updated.Status.Checkpoints).To(HaveLen(1))
			Expect(updated.Status.Checkpoints[0].Name).To(Equal(tracker.Status.LatestCheckpoint.Name))
		})

		It("should not mutate the original tracker", func() {
			tracker = createTracker("tracker1", "test-vmi", true, false)
			_, err := kubevirtCli.BackupV1alpha1().VirtualMachineBackupTrackers(testNamespace).Create(
				context.Background(), tracker, metav1.CreateOptions{})
			Expect(err).ToNot(HaveOccurred())

			virtClient.EXPECT().VirtualMachineBackupTracker(testNamespace).
				Return(kubevirtCli.BackupV1alpha1().VirtualMachineBackupTrackers(testNamespace))

			originalCheckpointName := tracker.Status.LatestCheckpoint.Name
			backupStatus := &v1.VirtualMachineInstanceBackupStatus{
				CheckpointName: new("cp-new"),
				Volumes: []v1.VirtualMachineInstanceBackupVolumeInfo{
					{VolumeName: "rootdisk"},
				},
			}
			err = ctrl.updateBackupTracker(testNamespace, tracker, backupv1.Full, backupStatus)
			Expect(err).ToNot(HaveOccurred())

			Expect(tracker.Status.LatestCheckpoint.Name).To(Equal(originalCheckpointName))
		})
	})

	Context("trackerNeedsPruning", func() {
		DescribeTable("should correctly identify trackers needing pruning",
			func(tracker *backupv1.VirtualMachineBackupTracker, expected bool) {
				Expect(trackerNeedsPruning(tracker)).To(Equal(expected))
			},
			Entry("nil tracker", nil, false),
			Entry("nil status", &backupv1.VirtualMachineBackupTracker{
				Spec: backupv1.VirtualMachineBackupTrackerSpec{},
			}, false),
			Entry("nil RetainCheckpoints", &backupv1.VirtualMachineBackupTracker{
				Spec:   backupv1.VirtualMachineBackupTrackerSpec{},
				Status: &backupv1.VirtualMachineBackupTrackerStatus{Checkpoints: []backupv1.BackupCheckpoint{{Name: "cp-1"}}},
			}, false),
			Entry("checkpoints within retain limit", &backupv1.VirtualMachineBackupTracker{
				Spec:   backupv1.VirtualMachineBackupTrackerSpec{RetainCheckpoints: new(int32(2))},
				Status: &backupv1.VirtualMachineBackupTrackerStatus{Checkpoints: []backupv1.BackupCheckpoint{{Name: "cp-1"}, {Name: "cp-2"}}},
			}, false),
			Entry("checkpoints exceed retain limit", &backupv1.VirtualMachineBackupTracker{
				Spec:   backupv1.VirtualMachineBackupTrackerSpec{RetainCheckpoints: new(int32(1))},
				Status: &backupv1.VirtualMachineBackupTrackerStatus{Checkpoints: []backupv1.BackupCheckpoint{{Name: "cp-1"}, {Name: "cp-2"}}},
			}, true),
			Entry("empty checkpoints with retain=0", &backupv1.VirtualMachineBackupTracker{
				Spec:   backupv1.VirtualMachineBackupTrackerSpec{RetainCheckpoints: new(int32(0))},
				Status: &backupv1.VirtualMachineBackupTrackerStatus{},
			}, false),
		)
	})

	Context("pruneExcessCheckpoints", func() {
		var (
			ctrl         *VMBackupController
			vmiInterface *kubecli.MockVirtualMachineInstanceInterface
		)

		BeforeEach(func() {
			vmiInterface = kubecli.NewMockVirtualMachineInstanceInterface(mockCtrl)
			pruneBackupInformer, _ := testutils.NewFakeInformerWithIndexersFor(
				&backupv1.VirtualMachineBackup{},
				controller.GetVirtualMachineBackupInformerIndexers(),
			)
			ctrl = &VMBackupController{
				client:         virtClient,
				backupInformer: pruneBackupInformer,
			}
		})

		It("should delete excess checkpoints via DeleteCheckpoint RPC", func() {
			tracker := &backupv1.VirtualMachineBackupTracker{
				ObjectMeta: metav1.ObjectMeta{Name: "tracker1", Namespace: testNamespace},
				Spec: backupv1.VirtualMachineBackupTrackerSpec{
					Source:            corev1.TypedLocalObjectReference{Name: "test-vmi"},
					RetainCheckpoints: new(int32(1)),
				},
				Status: &backupv1.VirtualMachineBackupTrackerStatus{
					Checkpoints: []backupv1.BackupCheckpoint{
						{Name: "cp-old"},
						{Name: "cp-mid"},
						{Name: "cp-latest"},
					},
				},
			}

			virtClient.EXPECT().VirtualMachineInstance(testNamespace).Return(vmiInterface).Times(2)
			vmiInterface.EXPECT().DeleteCheckpoint(gomock.Any(), "test-vmi", "cp-old").Return(nil)
			vmiInterface.EXPECT().DeleteCheckpoint(gomock.Any(), "test-vmi", "cp-mid").Return(nil)

			err := ctrl.pruneExcessCheckpoints(tracker)
			Expect(err).ToNot(HaveOccurred())
			Expect(tracker.Status.Checkpoints).To(HaveLen(1))
			Expect(tracker.Status.Checkpoints[0].Name).To(Equal("cp-latest"))
		})

		It("should persist partial progress on failure", func() {
			tracker := &backupv1.VirtualMachineBackupTracker{
				ObjectMeta: metav1.ObjectMeta{Name: "tracker1", Namespace: testNamespace},
				Spec: backupv1.VirtualMachineBackupTrackerSpec{
					Source:            corev1.TypedLocalObjectReference{Name: "test-vmi"},
					RetainCheckpoints: new(int32(1)),
				},
				Status: &backupv1.VirtualMachineBackupTrackerStatus{
					Checkpoints: []backupv1.BackupCheckpoint{
						{Name: "cp-1"},
						{Name: "cp-2"},
						{Name: "cp-3"},
					},
				},
			}

			gomock.InOrder(
				virtClient.EXPECT().VirtualMachineInstance(testNamespace).Return(vmiInterface),
				vmiInterface.EXPECT().DeleteCheckpoint(gomock.Any(), "test-vmi", "cp-1").Return(nil),
				virtClient.EXPECT().VirtualMachineInstance(testNamespace).Return(vmiInterface),
				vmiInterface.EXPECT().DeleteCheckpoint(gomock.Any(), "test-vmi", "cp-2").Return(errors.New("RPC failed")),
			)

			err := ctrl.pruneExcessCheckpoints(tracker)
			Expect(err).To(HaveOccurred())
			Expect(err.Error()).To(ContainSubstring("failed to delete checkpoint cp-2"))
			Expect(tracker.Status.Checkpoints).To(HaveLen(2))
			Expect(tracker.Status.Checkpoints[0].Name).To(Equal("cp-2"))
			Expect(tracker.Status.Checkpoints[1].Name).To(Equal("cp-3"))
		})

		It("should be a no-op when RetainCheckpoints is nil", func() {
			tracker := &backupv1.VirtualMachineBackupTracker{
				ObjectMeta: metav1.ObjectMeta{Name: "tracker1", Namespace: testNamespace},
				Spec: backupv1.VirtualMachineBackupTrackerSpec{
					Source: corev1.TypedLocalObjectReference{Name: "test-vmi"},
				},
				Status: &backupv1.VirtualMachineBackupTrackerStatus{
					Checkpoints: []backupv1.BackupCheckpoint{
						{Name: "cp-1"},
						{Name: "cp-2"},
					},
				},
			}

			err := ctrl.pruneExcessCheckpoints(tracker)
			Expect(err).ToNot(HaveOccurred())
			Expect(tracker.Status.Checkpoints).To(HaveLen(2))
		})

		It("should be a no-op when checkpoints are within retain limit", func() {
			tracker := &backupv1.VirtualMachineBackupTracker{
				ObjectMeta: metav1.ObjectMeta{Name: "tracker1", Namespace: testNamespace},
				Spec: backupv1.VirtualMachineBackupTrackerSpec{
					Source:            corev1.TypedLocalObjectReference{Name: "test-vmi"},
					RetainCheckpoints: new(int32(3)),
				},
				Status: &backupv1.VirtualMachineBackupTrackerStatus{
					Checkpoints: []backupv1.BackupCheckpoint{
						{Name: "cp-1"},
						{Name: "cp-2"},
					},
				},
			}

			err := ctrl.pruneExcessCheckpoints(tracker)
			Expect(err).ToNot(HaveOccurred())
			Expect(tracker.Status.Checkpoints).To(HaveLen(2))
		})
	})

	Context("executeTracker with pruning", func() {
		var (
			ctrl            *VMBackupController
			trackerInformer cache.SharedIndexInformer
			backupInformer  cache.SharedIndexInformer
			vmiInformer     cache.SharedIndexInformer
			recorder        *record.FakeRecorder
			vmiInterface    *kubecli.MockVirtualMachineInstanceInterface
		)

		BeforeEach(func() {
			trackerInformer, _ = testutils.NewFakeInformerWithIndexersFor(
				&backupv1.VirtualMachineBackupTracker{},
				controller.GetVirtualMachineBackupTrackerInformerIndexers(),
			)
			backupInformer, _ = testutils.NewFakeInformerWithIndexersFor(
				&backupv1.VirtualMachineBackup{},
				controller.GetVirtualMachineBackupInformerIndexers(),
			)
			vmiInformer, _ = testutils.NewFakeInformerFor(&v1.VirtualMachineInstance{})
			recorder = record.NewFakeRecorder(100)
			recorder.IncludeObject = true
			vmiInterface = kubecli.NewMockVirtualMachineInstanceInterface(mockCtrl)

			ctrl = &VMBackupController{
				client:                virtClient,
				backupTrackerInformer: trackerInformer,
				backupInformer:        backupInformer,
				vmiStore:              vmiInformer.GetStore(),
				recorder:              recorder,
			}
		})

		It("should prune excess checkpoints via executeTracker when no redefinition needed", func() {
			tracker := createTracker("tracker1", "test-vmi", false, false)
			tracker.Spec.RetainCheckpoints = new(int32(1))
			tracker.Status = &backupv1.VirtualMachineBackupTrackerStatus{
				Checkpoints: []backupv1.BackupCheckpoint{
					{Name: "cp-old"},
					{Name: "cp-latest"},
				},
			}
			Expect(trackerInformer.GetStore().Add(tracker)).To(Succeed())
			_, err := kubevirtCli.BackupV1alpha1().VirtualMachineBackupTrackers(testNamespace).Create(
				context.Background(), tracker, metav1.CreateOptions{})
			Expect(err).ToNot(HaveOccurred())

			testVMI := libvmi.New(libvmi.WithNamespace(testNamespace), libvmi.WithName("test-vmi"))
			Expect(vmiInformer.GetStore().Add(testVMI)).To(Succeed())

			virtClient.EXPECT().VirtualMachineInstance(testNamespace).Return(vmiInterface)
			vmiInterface.EXPECT().DeleteCheckpoint(gomock.Any(), "test-vmi", "cp-old").Return(nil)
			virtClient.EXPECT().VirtualMachineBackupTracker(testNamespace).
				Return(kubevirtCli.BackupV1alpha1().VirtualMachineBackupTrackers(testNamespace))

			err = ctrl.executeTracker(testNamespace + "/tracker1")
			Expect(err).ToNot(HaveOccurred())

			updated, err := kubevirtCli.BackupV1alpha1().VirtualMachineBackupTrackers(testNamespace).Get(
				context.Background(), "tracker1", metav1.GetOptions{})
			Expect(err).ToNot(HaveOccurred())
			Expect(updated.Status.Checkpoints).To(HaveLen(1))
			Expect(updated.Status.Checkpoints[0].Name).To(Equal("cp-latest"))
		})
	})
})

func createTracker(name, vmName string, hasCheckpoint bool, redefinitionRequired bool) *backupv1.VirtualMachineBackupTracker {
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
	}
	if hasCheckpoint {
		tracker.Status = &backupv1.VirtualMachineBackupTrackerStatus{
			Checkpoints: []backupv1.BackupCheckpoint{{
				Name: "checkpoint-1",
			}},
			CheckpointRedefinitionRequired: pointer.P(redefinitionRequired),
		}
	}
	return tracker
}
