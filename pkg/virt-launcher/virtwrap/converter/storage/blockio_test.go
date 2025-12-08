package storage_test

import (
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	v1 "kubevirt.io/api/core/v1"
	"kubevirt.io/kubevirt/pkg/libvmi"
	libvmistatus "kubevirt.io/kubevirt/pkg/libvmi/status"
	"kubevirt.io/kubevirt/pkg/pointer"
	"kubevirt.io/kubevirt/pkg/virt-launcher/virtwrap/api"
	"kubevirt.io/kubevirt/pkg/virt-launcher/virtwrap/converter/storage"
)

var _ = Describe("Storage Configurator", func() {
	Context("BlockIO", func() {
		It("Should add blockio fields when custom sizes are provided", func() {
			configurator := storage.NewDomainConfigurator(
				storage.WithArchitecture("amd64"),
			)

			vmi := libvmi.New(
				libvmi.WithDataVolume(blockDVName, blockPVCName, withCustomBlockSize(1234, 1234, 1234)),
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

			Expect(disk.BlockIO).ToNot(BeNil())

			Expect(disk.BlockIO.LogicalBlockSize).To(Equal(uint(1234)))
			Expect(disk.BlockIO.PhysicalBlockSize).To(Equal(uint(1234)))
			Expect(*disk.BlockIO.DiscardGranularity).To(Equal(uint(1234)))
		})
	})

})

func withCustomBlockSize(logical, physical uint, discardGranularity uint) libvmi.DiskOption {
	return func(disk *v1.Disk) {
		disk.BlockSize = &v1.BlockSize{
			Custom: &v1.CustomBlockSize{
				Logical:            logical,
				Physical:           physical,
				DiscardGranularity: pointer.P(discardGranularity),
			},
		}
	}
}
