package storage_test

import (
	"fmt"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	v1 "kubevirt.io/api/core/v1"
	"kubevirt.io/kubevirt/pkg/libvmi"
	libvmistatus "kubevirt.io/kubevirt/pkg/libvmi/status"
	"kubevirt.io/kubevirt/pkg/pointer"
	"kubevirt.io/kubevirt/pkg/virt-launcher/virtwrap/api"
	"kubevirt.io/kubevirt/pkg/virt-launcher/virtwrap/converter/storage"

	k8sv1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
)

const (
	blockPVCName = "pvc_block_test"
	blockDVName  = "dv_block_test"
)

var _ = Describe("Storage Configurator", func() {

	Context("Disk Configurator", func() {

		DescribeTable("Should define disk capacity as the minimum of capacity and request", func(arch string, requests, capacity, expected int64) {
			configurator := storage.NewDomainConfigurator(
				storage.WithDiskConfigurator(
					storage.NewDiskConfigurator(
						storage.WithDiskArchitecture(arch),
					),
				),
			)
			vmi := libvmi.New(
				libvmi.WithDataVolume(blockDVName, blockPVCName),
				libvmistatus.WithStatus(
					libvmistatus.New(
						libvmistatus.WithPhase(v1.Running),
						libvmistatus.WithVolumeStatus(
							v1.VolumeStatus{
								Name: blockDVName,
								PersistentVolumeClaimInfo: &v1.PersistentVolumeClaimInfo{
									Capacity: k8sv1.ResourceList{
										k8sv1.ResourceStorage: *resource.NewQuantity(capacity, resource.DecimalSI),
									},
									Requests: k8sv1.ResourceList{
										k8sv1.ResourceStorage: *resource.NewQuantity(requests, resource.DecimalSI),
									},
								},
							},
						),
					),
				),
			)
			var domain api.Domain

			Expect(configurator.Configure(vmi, &domain)).To(Succeed())
			Expect(domain.Spec.Devices.Disks[0].Capacity).ToNot(BeNil())
			Expect(*domain.Spec.Devices.Disks[0].Capacity).To(Equal(expected))
		},
			MultiArchEntry("Higher request than capacity", int64(9999), int64(1111), int64(1111)),
			MultiArchEntry("Lower request than capacity", int64(1111), int64(9999), int64(1111)),
		)

		DescribeTable("Should assign scsi controller to", func(diskDevice v1.DiskDevice) {
			configurator := storage.NewDomainConfigurator(
				storage.WithDiskConfigurator(
					storage.NewDiskConfigurator(
						storage.WithDiskArchitecture("amd64"),
					),
				),
			)

			vmi := libvmi.New(
				libvmi.WithDataVolume(blockDVName, blockPVCName),
				libvmistatus.WithStatus(
					libvmistatus.New(
						libvmistatus.WithPhase(v1.Running),
						libvmistatus.WithVolumeStatus(
							v1.VolumeStatus{
								Name: blockDVName,
							},
						),
					),
				),
			)
			vmi.Spec.Domain.Devices.Disks[0].DiskDevice = diskDevice

			domain := &api.Domain{}
			Expect(configurator.Configure(vmi, domain)).To(Succeed())

			Expect(domain.Spec.Devices.Disks).To(HaveLen(1))
			disk := domain.Spec.Devices.Disks[0]

			Expect(disk.Address).ToNot(BeNil())
			Expect(disk.Address.Bus).To(Equal("0"))
			Expect(disk.Address.Controller).To(Equal("0"))
			Expect(disk.Address.Type).To(Equal("drive"))
			Expect(disk.Address.Unit).To(Equal("0"))
		},
			Entry("LUN-type disk", v1.DiskDevice{
				LUN: &v1.LunTarget{Bus: "scsi"},
			}),
			Entry("Disk-type disk", v1.DiskDevice{
				Disk: &v1.DiskTarget{Bus: "scsi"},
			}),
		)

		DescribeTable("Should add boot order when provided", func(arch, expectedModel string) {
			configurator := storage.NewDomainConfigurator(
				storage.WithDiskConfigurator(
					storage.NewDiskConfigurator(
						storage.WithDiskArchitecture("amd64"),
					),
				),
			)

			vmi := libvmi.New(
				libvmi.WithDataVolume(blockDVName, blockPVCName, withBootOrder(1)),
				libvmistatus.WithStatus(
					libvmistatus.New(
						libvmistatus.WithPhase(v1.Running),
						libvmistatus.WithVolumeStatus(
							v1.VolumeStatus{Name: blockDVName},
						),
					),
				),
			)

			domain := &api.Domain{}
			Expect(configurator.Configure(vmi, domain)).To(Succeed())

			Expect(domain.Spec.Devices.Disks).To(HaveLen(1))
			disk := domain.Spec.Devices.Disks[0]

			Expect(disk.BootOrder).ToNot(BeNil())
			Expect(disk.BootOrder.Order).To(Equal(uint(1)))

			Expect(disk.Model).To(Equal(expectedModel))
		},
			Entry("on amd64", "amd64", "virtio-non-transitional"),
			Entry("on arm64", "arm64", "virtio-non-transitional"),
			Entry("on s390x", "s390x", "virtio"),
		)

		DescribeTable("should set disk I/O mode if requested", func(arch string) {
			configurator := storage.NewDomainConfigurator(
				storage.WithDiskConfigurator(
					storage.NewDiskConfigurator(
						storage.WithDiskArchitecture("amd64"),
					),
				),
			)

			vmi := libvmi.New(
				libvmi.WithDataVolume(blockDVName, blockPVCName, withIOMode("native")),
				libvmistatus.WithStatus(
					libvmistatus.New(
						libvmistatus.WithPhase(v1.Running),
						libvmistatus.WithVolumeStatus(
							v1.VolumeStatus{Name: blockDVName},
						),
					),
				),
			)

			domain := &api.Domain{}
			Expect(configurator.Configure(vmi, domain)).To(Succeed())

			Expect(domain.Spec.Devices.Disks).To(HaveLen(1))
			disk := domain.Spec.Devices.Disks[0]

			Expect(disk.Driver).ToNot(BeNil())
			Expect(disk.Driver.IO).To(Equal(v1.DriverIO("native")))
		},
			MultiArchEntry(""),
		)

		DescribeTable("should not set disk I/O mode if not requested", func(arch string) {
			configurator := storage.NewDomainConfigurator(
				storage.WithDiskConfigurator(
					storage.NewDiskConfigurator(
						storage.WithDiskArchitecture("amd64"),
					),
				),
			)

			vmi := libvmi.New(
				libvmi.WithDataVolume(blockDVName, blockPVCName),
				libvmistatus.WithStatus(
					libvmistatus.New(
						libvmistatus.WithPhase(v1.Running),
						libvmistatus.WithVolumeStatus(
							v1.VolumeStatus{Name: blockDVName},
						),
					),
				),
			)

			domain := &api.Domain{}
			Expect(configurator.Configure(vmi, domain)).To(Succeed())

			Expect(domain.Spec.Devices.Disks).To(HaveLen(1))
			disk := domain.Spec.Devices.Disks[0]

			Expect(disk.Driver).ToNot(BeNil())
			Expect(disk.Driver.IO).To(BeEmpty())
		},
			MultiArchEntry(""),
		)

		DescribeTable("Should omit boot order when not provided", func(arch, expectedModel string) {
			configurator := storage.NewDomainConfigurator(
				storage.WithDiskConfigurator(
					storage.NewDiskConfigurator(
						storage.WithDiskArchitecture("amd64"),
					),
				),
			)

			vmi := libvmi.New(
				libvmi.WithDataVolume(blockDVName, blockPVCName),
				libvmistatus.WithStatus(
					libvmistatus.New(
						libvmistatus.WithPhase(v1.Running),
						libvmistatus.WithVolumeStatus(
							v1.VolumeStatus{Name: blockDVName},
						),
					),
				),
			)

			domain := &api.Domain{}
			Expect(configurator.Configure(vmi, domain)).To(Succeed())

			Expect(domain.Spec.Devices.Disks).To(HaveLen(1))
			disk := domain.Spec.Devices.Disks[0]

			Expect(disk.BootOrder).To(BeNil())

			Expect(disk.Model).To(Equal(expectedModel))
		},
			Entry("on amd64", "amd64", "virtio-non-transitional"),
			Entry("on arm64", "arm64", "virtio-non-transitional"),
			Entry("on s390x", "s390x", "virtio"),
		)

		//
		//		It("Should add blockio fields when custom sizes are provided", func() {
		//			kubevirtDisk := &v1.Disk{
		//				BlockSize: &v1.BlockSize{
		//					Custom: &v1.CustomBlockSize{
		//						Logical:            1234,
		//						Physical:           1234,
		//						DiscardGranularity: pointer.P[uint](1234),
		//					},
		//				},
		//			}
		//			expectedXML := `<Disk device="" type="">
		//  <source></source>
		//  <target></target>
		//  <blockio logical_block_size="1234" physical_block_size="1234" discard_granularity="1234"></blockio>
		//</Disk>`
		//			libvirtDisk := &api.Disk{}
		//			Expect(Convert_v1_BlockSize_To_api_BlockIO(kubevirtDisk, libvirtDisk)).To(Succeed())
		//			data, err := xml.MarshalIndent(libvirtDisk, "", "  ")
		//			Expect(err).ToNot(HaveOccurred())
		//			xml := string(data)
		//			Expect(xml).To(Equal(expectedXML))
		//		})
		//

		DescribeTable("should set sharable and the cache if requested", func(arch, expectedModel string) {
			configurator := storage.NewDomainConfigurator(
				storage.WithDiskConfigurator(
					storage.NewDiskConfigurator(
						storage.WithDiskArchitecture("amd64"),
					),
				),
			)

			vmi := libvmi.New(
				libvmi.WithDataVolume(blockDVName, blockPVCName, withShareable()),
				libvmistatus.WithStatus(
					libvmistatus.New(
						libvmistatus.WithPhase(v1.Running),
						libvmistatus.WithVolumeStatus(
							v1.VolumeStatus{Name: blockDVName},
						),
					),
				),
			)

			domain := &api.Domain{}
			Expect(configurator.Configure(vmi, domain)).To(Succeed())

			Expect(domain.Spec.Devices.Disks).To(HaveLen(1))
			disk := domain.Spec.Devices.Disks[0]

			// Verify Shareable field is present (not nil)
			Expect(disk.Shareable).ToNot(BeNil())

			// Verify Cache is forced to "none"
			Expect(disk.Driver.Cache).To(Equal("none"))

			// Verify Model
			Expect(disk.Model).To(Equal(expectedModel))
		},
			Entry("on amd64", "amd64", "virtio-non-transitional"),
			Entry("on arm64", "arm64", "virtio-non-transitional"),
			Entry("on s390x", "s390x", "virtio"),
		)
	})

})

const (
	amd64 = "amd64"
	arm64 = "arm64"
	s390x = "s390x"
)

// MultiArchEntry returns a slice of Ginkgo TableEntry starting from one.
// It repeats the same TableEntry for every architecture.
// This is pretty useful when the same behavior is expected for every arch.
// **IMPORTANT**
// This requires the DescribeTable body func to have `arch string` as first
// parameter.

func MultiArchEntry(text string, args ...any) []TableEntry {
	return []TableEntry{
		Entry(fmt.Sprintf("%s on %s", text, amd64), append([]any{amd64}, args...)...),
		Entry(fmt.Sprintf("%s on %s", text, arm64), append([]any{arm64}, args...)...),
		Entry(fmt.Sprintf("%s on %s", text, s390x), append([]any{s390x}, args...)...),
	}
}

func withBootOrder(bootOrder uint) libvmi.DiskOption {
	return func(disk *v1.Disk) {
		disk.BootOrder = &bootOrder
	}
}

func withIOMode(ioMode v1.DriverIO) libvmi.DiskOption {
	return func(disk *v1.Disk) {
		disk.IO = ioMode
	}
}

func withShareable() libvmi.DiskOption {
	return func(disk *v1.Disk) {
		disk.Shareable = pointer.P(true)
	}
}
