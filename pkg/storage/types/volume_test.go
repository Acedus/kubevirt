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

package types

import (
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	k8sv1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"

	v1 "kubevirt.io/api/core/v1"
)

var _ = Describe("Volume type test", func() {

	Context("GetTotalSizeMigratedVolumes", func() {
		It("should return 0 when no migrated volumes", func() {
			vmi := &v1.VirtualMachineInstance{
				Status: v1.VirtualMachineInstanceStatus{
					MigratedVolumes: []v1.StorageMigratedVolumeInfo{},
				},
			}
			result := GetTotalSizeMigratedVolumes(vmi)
			Expect(result.Value()).To(Equal(int64(0)))
		})

		It("should return 0 when SourcePVCInfo is nil", func() {
			vmi := &v1.VirtualMachineInstance{
				Status: v1.VirtualMachineInstanceStatus{
					MigratedVolumes: []v1.StorageMigratedVolumeInfo{
						{
							VolumeName:    "test-vol",
							SourcePVCInfo: nil,
						},
					},
				},
			}
			result := GetTotalSizeMigratedVolumes(vmi)
			Expect(result.Value()).To(Equal(int64(0)))
		})

		It("should calculate size correctly for 10Gi volume using SourcePVCInfo", func() {
			tenGi := resource.MustParse("10Gi")
			vmi := &v1.VirtualMachineInstance{
				Status: v1.VirtualMachineInstanceStatus{
					MigratedVolumes: []v1.StorageMigratedVolumeInfo{
						{
							VolumeName: "test-vol",
							SourcePVCInfo: &v1.PersistentVolumeClaimInfo{
								ClaimName: "source-pvc",
								Capacity: k8sv1.ResourceList{
									k8sv1.ResourceStorage: tenGi,
								},
								Requests: k8sv1.ResourceList{
									k8sv1.ResourceStorage: tenGi,
								},
							},
							DestinationPVCInfo: &v1.PersistentVolumeClaimInfo{
								ClaimName: "dest-pvc",
							},
						},
					},
				},
			}
			result := GetTotalSizeMigratedVolumes(vmi)
			Expect(result.Value()).To(Equal(tenGi.Value()))
		})

		It("should sum multiple migrated volumes", func() {
			tenGi := resource.MustParse("10Gi")
			fiveGi := resource.MustParse("5Gi")
			vmi := &v1.VirtualMachineInstance{
				Status: v1.VirtualMachineInstanceStatus{
					MigratedVolumes: []v1.StorageMigratedVolumeInfo{
						{
							VolumeName: "vol1",
							SourcePVCInfo: &v1.PersistentVolumeClaimInfo{
								ClaimName: "source-pvc-1",
								Capacity: k8sv1.ResourceList{
									k8sv1.ResourceStorage: tenGi,
								},
								Requests: k8sv1.ResourceList{
									k8sv1.ResourceStorage: tenGi,
								},
							},
						},
						{
							VolumeName: "vol2",
							SourcePVCInfo: &v1.PersistentVolumeClaimInfo{
								ClaimName: "source-pvc-2",
								Capacity: k8sv1.ResourceList{
									k8sv1.ResourceStorage: fiveGi,
								},
								Requests: k8sv1.ResourceList{
									k8sv1.ResourceStorage: fiveGi,
								},
							},
						},
					},
				},
			}
			result := GetTotalSizeMigratedVolumes(vmi)
			expectedSize := tenGi.Value() + fiveGi.Value()
			Expect(result.Value()).To(Equal(expectedSize))
		})
	})
})
