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

package hotplug_test

import (
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	k8sv1 "k8s.io/api/core/v1"

	v1 "kubevirt.io/api/core/v1"

	"kubevirt.io/kubevirt/pkg/libvmi"
	"kubevirt.io/kubevirt/pkg/storage/hotplug"
)

var _ = Describe("Hotplug volume descriptor", func() {
	withMemoryDumpVolume := func(volumeName, claimName string) libvmi.Option {
		return func(vmi *v1.VirtualMachineInstance) {
			vmi.Spec.Volumes = append(vmi.Spec.Volumes, v1.Volume{
				Name: volumeName,
				VolumeSource: v1.VolumeSource{
					MemoryDump: &v1.MemoryDumpVolumeSource{
						PersistentVolumeClaimVolumeSource: v1.PersistentVolumeClaimVolumeSource{
							PersistentVolumeClaimVolumeSource: k8sv1.PersistentVolumeClaimVolumeSource{ClaimName: claimName},
							Hotpluggable:                      true,
						},
					},
				},
			})
		}
	}

	withUtilityVolume := func(volumeName, claimName string, volumeType *v1.UtilityVolumeType) libvmi.Option {
		return func(vmi *v1.VirtualMachineInstance) {
			vmi.Spec.UtilityVolumes = append(vmi.Spec.UtilityVolumes, v1.UtilityVolume{
				Name:                              volumeName,
				PersistentVolumeClaimVolumeSource: k8sv1.PersistentVolumeClaimVolumeSource{ClaimName: claimName},
				Type:                              volumeType,
			})
		}
	}

	withContainerDisk := libvmi.WithContainerDisk("containerdisk", "image")

	launcherPodWithVolumes := func(names ...string) *k8sv1.Pod {
		pod := &k8sv1.Pod{}
		for _, name := range names {
			pod.Spec.Volumes = append(pod.Spec.Volumes, k8sv1.Volume{Name: name})
		}
		return pod
	}

	Context("SpecVolumes", func() {
		It("should skip volumes that are not PVC-backed", func() {
			vmi := libvmi.New(withContainerDisk)
			Expect(hotplug.SpecVolumes(&vmi.Spec)).To(BeEmpty())
		})

		DescribeTable("should classify volumes", func(expected []hotplug.Volume, opts ...libvmi.Option) {
			vmi := libvmi.New(opts...)
			Expect(hotplug.SpecVolumes(&vmi.Spec)).To(Equal(expected))
		},
			Entry("with a PVC as a disk",
				[]hotplug.Volume{{Name: "disk", ClaimName: "pvc", Kind: hotplug.KindDisk}},
				libvmi.WithPersistentVolumeClaim("disk", "pvc"),
			),
			Entry("with a hotplug PVC as a disk",
				[]hotplug.Volume{{Name: "disk", ClaimName: "pvc", Kind: hotplug.KindDisk}},
				libvmi.WithHotplugPersistentVolumeClaim("disk", "pvc"),
			),
			Entry("with a DataVolume as a disk claimed by its DataVolume name",
				[]hotplug.Volume{{Name: "disk", ClaimName: "dv", Kind: hotplug.KindDisk}},
				libvmi.WithDataVolume("disk", "dv"),
			),
			Entry("with a hotplug DataVolume as a disk",
				[]hotplug.Volume{{Name: "disk", ClaimName: "dv", Kind: hotplug.KindDisk}},
				libvmi.WithHotplugDataVolume("disk", "dv"),
			),
			Entry("with a memory dump volume as a directory",
				[]hotplug.Volume{{Name: "dump", ClaimName: "dump-pvc", Kind: hotplug.KindDirectory}},
				withMemoryDumpVolume("dump", "dump-pvc"),
			),
			Entry("with an untyped utility volume as a directory",
				[]hotplug.Volume{{Name: "utility", ClaimName: "utility-pvc", Kind: hotplug.KindDirectory}},
				withUtilityVolume("utility", "utility-pvc", nil),
			),
			Entry("with a backup utility volume as a directory",
				[]hotplug.Volume{{Name: "backup", ClaimName: "backup-pvc", Kind: hotplug.KindDirectory}},
				withUtilityVolume("backup", "backup-pvc", new(v1.Backup)),
			),
			Entry("with a memory dump utility volume as a directory",
				[]hotplug.Volume{{Name: "dump", ClaimName: "dump-pvc", Kind: hotplug.KindDirectory}},
				withUtilityVolume("dump", "dump-pvc", new(v1.MemoryDump)),
			),
			Entry("with spec volumes before utility volumes, each in spec order",
				[]hotplug.Volume{
					{Name: "disk-b", ClaimName: "pvc-b", Kind: hotplug.KindDisk},
					{Name: "disk-a", ClaimName: "dv-a", Kind: hotplug.KindDisk},
					{Name: "utility-b", ClaimName: "utility-pvc-b", Kind: hotplug.KindDirectory},
					{Name: "utility-a", ClaimName: "utility-pvc-a", Kind: hotplug.KindDirectory},
				},
				withUtilityVolume("utility-b", "utility-pvc-b", nil),
				libvmi.WithPersistentVolumeClaim("disk-b", "pvc-b"),
				withContainerDisk,
				withUtilityVolume("utility-a", "utility-pvc-a", nil),
				libvmi.WithDataVolume("disk-a", "dv-a"),
			),
		)
	})

	Context("SpecVolumesByName", func() {
		It("should index every spec volume by name", func() {
			vmi := libvmi.New(
				libvmi.WithPersistentVolumeClaim("disk", "pvc"),
				withContainerDisk,
				withUtilityVolume("utility", "utility-pvc", nil),
			)
			Expect(hotplug.SpecVolumesByName(&vmi.Spec)).To(Equal(map[string]hotplug.Volume{
				"disk":    {Name: "disk", ClaimName: "pvc", Kind: hotplug.KindDisk},
				"utility": {Name: "utility", ClaimName: "utility-pvc", Kind: hotplug.KindDirectory},
			}))
		})
	})

	Context("VolumesToAttach", func() {
		DescribeTable("should return volumes not served by the launcher pod", func(launcherPod *k8sv1.Pod, expected []hotplug.Volume) {
			vmi := libvmi.New(
				libvmi.WithPersistentVolumeClaim("cold", "cold-pvc"),
				libvmi.WithHotplugDataVolume("hot", "hot-dv"),
				withMemoryDumpVolume("dump", "dump-pvc"),
				withUtilityVolume("utility", "utility-pvc", new(v1.Backup)),
			)
			Expect(hotplug.VolumesToAttach(vmi, launcherPod)).To(Equal(expected))
		},
			Entry("when the launcher pod serves none of them",
				launcherPodWithVolumes(),
				[]hotplug.Volume{
					{Name: "cold", ClaimName: "cold-pvc", Kind: hotplug.KindDisk},
					{Name: "hot", ClaimName: "hot-dv", Kind: hotplug.KindDisk},
					{Name: "dump", ClaimName: "dump-pvc", Kind: hotplug.KindDirectory},
					{Name: "utility", ClaimName: "utility-pvc", Kind: hotplug.KindDirectory},
				},
			),
			Entry("when the launcher pod serves the cold-plugged volume",
				launcherPodWithVolumes("cold", "hotplug-disks"),
				[]hotplug.Volume{
					{Name: "hot", ClaimName: "hot-dv", Kind: hotplug.KindDisk},
					{Name: "dump", ClaimName: "dump-pvc", Kind: hotplug.KindDirectory},
					{Name: "utility", ClaimName: "utility-pvc", Kind: hotplug.KindDirectory},
				},
			),
			Entry("when a utility volume name collides with a launcher pod volume",
				launcherPodWithVolumes("cold", "utility"),
				[]hotplug.Volume{
					{Name: "hot", ClaimName: "hot-dv", Kind: hotplug.KindDisk},
					{Name: "dump", ClaimName: "dump-pvc", Kind: hotplug.KindDirectory},
				},
			),
		)

		It("should return no volumes when the launcher pod serves all of them", func() {
			vmi := libvmi.New(
				libvmi.WithPersistentVolumeClaim("cold", "cold-pvc"),
				withUtilityVolume("utility", "utility-pvc", nil),
			)
			Expect(hotplug.VolumesToAttach(vmi, launcherPodWithVolumes("cold", "utility"))).To(BeEmpty())
		})
	})
})
