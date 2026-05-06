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

package storage_test

import (
	"encoding/xml"
	"fmt"
	"path/filepath"
	"runtime"
	"strconv"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	k8sv1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	k8smeta "k8s.io/apimachinery/pkg/apis/meta/v1"

	v1 "kubevirt.io/api/core/v1"

	"kubevirt.io/kubevirt/pkg/ephemeral-disk/fake"
	"kubevirt.io/kubevirt/pkg/libvmi"
	"kubevirt.io/kubevirt/pkg/pointer"
	"kubevirt.io/kubevirt/pkg/virt-launcher/virtwrap/api"
	archconverter "kubevirt.io/kubevirt/pkg/virt-launcher/virtwrap/converter/arch"
	"kubevirt.io/kubevirt/pkg/virt-launcher/virtwrap/converter/storage"
	convertertypes "kubevirt.io/kubevirt/pkg/virt-launcher/virtwrap/converter/types"
)

func diskToDiskXML(arch string, disk *v1.Disk) string {
	devicePerBus := make(map[string]storage.DeviceNamer)
	libvirtDisk := &api.Disk{}
	Expect(storage.Convert_v1_Disk_To_api_Disk(&convertertypes.ConverterContext{Architecture: archconverter.NewConverter(arch), UseVirtioTransitional: false}, disk, libvirtDisk, devicePerBus, nil, make(map[string]v1.VolumeStatus))).To(Succeed())
	data, err := xml.MarshalIndent(libvirtDisk, "", "  ")
	Expect(err).ToNot(HaveOccurred())
	return string(data)
}

var _ = Describe("Convert_v1_Disk_To_api_Disk", func() {
	DescribeTable("Should define disk capacity as the minimum of capacity and request", func(arch string, requests, capacity, expected int64) {
		context := &convertertypes.ConverterContext{Architecture: archconverter.NewConverter(arch)}
		v1Disk := v1.Disk{
			Name: "myvolume",
			DiskDevice: v1.DiskDevice{
				Disk: &v1.DiskTarget{Bus: v1.VirtIO},
			},
		}
		apiDisk := api.Disk{}
		devicePerBus := map[string]storage.DeviceNamer{}
		numQueues := uint(2)
		volumeStatusMap := make(map[string]v1.VolumeStatus)
		volumeStatusMap["myvolume"] = v1.VolumeStatus{
			PersistentVolumeClaimInfo: &v1.PersistentVolumeClaimInfo{
				Capacity: k8sv1.ResourceList{
					k8sv1.ResourceStorage: *resource.NewQuantity(capacity, resource.DecimalSI),
				},
				Requests: k8sv1.ResourceList{
					k8sv1.ResourceStorage: *resource.NewQuantity(requests, resource.DecimalSI),
				},
			},
		}
		storage.Convert_v1_Disk_To_api_Disk(context, &v1Disk, &apiDisk, devicePerBus, &numQueues, volumeStatusMap)
		Expect(apiDisk.Capacity).ToNot(BeNil())
		Expect(*apiDisk.Capacity).To(Equal(expected))
	},
		Entry("Higher request than capacity on amd64", amd64, int64(9999), int64(1111), int64(1111)),
		Entry("Higher request than capacity on arm64", arm64, int64(9999), int64(1111), int64(1111)),
		Entry("Higher request than capacity on s390x", s390x, int64(9999), int64(1111), int64(1111)),
		Entry("Lower request than capacity on amd64", amd64, int64(1111), int64(9999), int64(1111)),
		Entry("Lower request than capacity on arm64", arm64, int64(1111), int64(9999), int64(1111)),
		Entry("Lower request than capacity on s390x", s390x, int64(1111), int64(9999), int64(1111)),
	)

	DescribeTable("Should assign scsi controller to", func(diskDevice v1.DiskDevice) {
		context := &convertertypes.ConverterContext{}
		v1Disk := v1.Disk{
			Name:       "myvolume",
			DiskDevice: diskDevice,
		}
		apiDisk := api.Disk{}
		devicePerBus := map[string]storage.DeviceNamer{}
		numQueues := uint(2)
		volumeStatusMap := make(map[string]v1.VolumeStatus)
		volumeStatusMap["myvolume"] = v1.VolumeStatus{}
		storage.Convert_v1_Disk_To_api_Disk(context, &v1Disk, &apiDisk, devicePerBus, &numQueues, volumeStatusMap)
		Expect(apiDisk.Address).ToNot(BeNil())
		Expect(apiDisk.Address.Bus).To(Equal("0"))
		Expect(apiDisk.Address.Controller).To(Equal("0"))
		Expect(apiDisk.Address.Type).To(Equal("drive"))
		Expect(apiDisk.Address.Unit).To(Equal("0"))
	},
		Entry("LUN-type disk", v1.DiskDevice{
			LUN: &v1.LunTarget{Bus: "scsi"},
		}),
		Entry("Disk-type disk", v1.DiskDevice{
			Disk: &v1.DiskTarget{Bus: "scsi"},
		}),
	)

	DescribeTable("Should add boot order when provided", func(arch, expectedModel string) {
		order := uint(1)
		kubevirtDisk := &v1.Disk{
			Name:      "mydisk",
			BootOrder: &order,
			DiskDevice: v1.DiskDevice{
				Disk: &v1.DiskTarget{
					Bus: v1.VirtIO,
				},
			},
		}
		convertedDisk := fmt.Sprintf(`<Disk device="disk" type="" model="%s">
  <source></source>
  <target bus="virtio" dev="vda"></target>
  <driver name="qemu" type="" discard="unmap"></driver>
  <alias name="ua-mydisk"></alias>
  <boot order="1"></boot>
</Disk>`, expectedModel)
		xml := diskToDiskXML(arch, kubevirtDisk)
		Expect(xml).To(Equal(convertedDisk))
	},
		Entry("on amd64", amd64, "virtio-non-transitional"),
		Entry("on arm64", arm64, "virtio-non-transitional"),
		Entry("on s390x", s390x, "virtio"),
	)

	DescribeTable("should set disk I/O mode if requested", func(arch string) {
		v1Disk := &v1.Disk{
			IO: "native",
		}
		xml := diskToDiskXML(arch, v1Disk)
		expectedXML := `<Disk device="" type="">
  <source></source>
  <target></target>
  <driver io="native" name="qemu" type=""></driver>
  <alias name="ua-"></alias>
</Disk>`
		Expect(xml).To(Equal(expectedXML))
	},
		Entry("on amd64", amd64),
		Entry("on arm64", arm64),
		Entry("on s390x", s390x),
	)

	DescribeTable("should not set disk I/O mode if not requested", func(arch string) {
		v1Disk := &v1.Disk{}
		xml := diskToDiskXML(arch, v1Disk)
		expectedXML := `<Disk device="" type="">
  <source></source>
  <target></target>
  <driver name="qemu" type=""></driver>
  <alias name="ua-"></alias>
</Disk>`
		Expect(xml).To(Equal(expectedXML))
	},
		Entry("on amd64", amd64),
		Entry("on arm64", arm64),
		Entry("on s390x", s390x),
	)

	DescribeTable("Should omit boot order when not provided", func(arch, expectedModel string) {
		kubevirtDisk := &v1.Disk{
			Name: "mydisk",
			DiskDevice: v1.DiskDevice{
				Disk: &v1.DiskTarget{
					Bus: v1.VirtIO,
				},
			},
		}
		var convertedDisk = fmt.Sprintf(`<Disk device="disk" type="" model="%s">
  <source></source>
  <target bus="virtio" dev="vda"></target>
  <driver name="qemu" type="" discard="unmap"></driver>
  <alias name="ua-mydisk"></alias>
</Disk>`, expectedModel)
		xml := diskToDiskXML(arch, kubevirtDisk)
		Expect(xml).To(Equal(convertedDisk))
	},
		Entry("on amd64", amd64, "virtio-non-transitional"),
		Entry("on arm64", arm64, "virtio-non-transitional"),
		Entry("on s390x", s390x, "virtio"),
	)

	DescribeTable("should set sharable and the cache if requested", func(arch, expectedModel string) {
		v1Disk := &v1.Disk{
			Name: "mydisk",
			DiskDevice: v1.DiskDevice{
				Disk: &v1.DiskTarget{
					Bus: v1.VirtIO,
				},
			},
			Shareable: pointer.P(true),
		}
		var expectedXML = fmt.Sprintf(`<Disk device="disk" type="" model="%s">
  <source></source>
  <target bus="virtio" dev="vda"></target>
  <driver cache="none" name="qemu" type="" discard="unmap"></driver>
  <alias name="ua-mydisk"></alias>
  <shareable></shareable>
</Disk>`, expectedModel)
		xml := diskToDiskXML(arch, v1Disk)
		Expect(xml).To(Equal(expectedXML))
	},
		Entry("on amd64", amd64, "virtio-non-transitional"),
		Entry("on arm64", arm64, "virtio-non-transitional"),
		Entry("on s390x", s390x, "virtio"),
	)

	It("should assign queues to a device if requested", func() {
		expectedQueues := uint(2)

		v1Disk := v1.Disk{
			DiskDevice: v1.DiskDevice{
				Disk: &v1.DiskTarget{Bus: v1.VirtIO},
			},
		}
		apiDisk := api.Disk{}
		devicePerBus := map[string]storage.DeviceNamer{}
		numQueues := uint(2)
		storage.Convert_v1_Disk_To_api_Disk(&convertertypes.ConverterContext{Architecture: archconverter.NewConverter(amd64)}, &v1Disk, &apiDisk, devicePerBus, &numQueues, make(map[string]v1.VolumeStatus))
		Expect(apiDisk.Device).To(Equal("disk"), "expected disk device to be defined")
		Expect(*(apiDisk.Driver.Queues)).To(Equal(expectedQueues), "expected queues to be 2")
	})

	It("should not assign queues to a device if omitted", func() {
		v1Disk := v1.Disk{
			DiskDevice: v1.DiskDevice{
				Disk: &v1.DiskTarget{},
			},
		}
		apiDisk := api.Disk{}
		devicePerBus := map[string]storage.DeviceNamer{}
		Expect(storage.Convert_v1_Disk_To_api_Disk(&convertertypes.ConverterContext{Architecture: archconverter.NewConverter(amd64)}, &v1Disk, &apiDisk, devicePerBus, nil, make(map[string]v1.VolumeStatus))).
			To(Succeed())
		Expect(apiDisk.Device).To(Equal("disk"), "expected disk device to be defined")
		Expect(apiDisk.Driver.Queues).To(BeNil(), "expected no queues to be requested")
	})

	It("should set disk pci address when specified", func() {
		disk := &v1.Disk{
			Name: "mydisk",
			DiskDevice: v1.DiskDevice{
				Disk: &v1.DiskTarget{
					Bus:        v1.DiskBusVirtio,
					PciAddress: "0000:81:01.0",
				},
			},
		}
		apiDisk := &api.Disk{}
		devicePerBus := map[string]storage.DeviceNamer{}
		Expect(storage.Convert_v1_Disk_To_api_Disk(
			&convertertypes.ConverterContext{Architecture: archconverter.NewConverter(amd64)},
			disk, apiDisk, devicePerBus, nil, make(map[string]v1.VolumeStatus),
		)).To(Succeed())
		Expect(apiDisk.Address).ToNot(BeNil())
		Expect(*apiDisk.Address).To(Equal(api.Address{
			Type:     api.AddressPCI,
			Domain:   "0x0000",
			Bus:      "0x81",
			Slot:     "0x01",
			Function: "0x0",
		}))
	})

	It("should fail when pci address is set with a non-virtio bus", func() {
		disk := &v1.Disk{
			Name: "mydisk",
			DiskDevice: v1.DiskDevice{
				Disk: &v1.DiskTarget{
					Bus:        "scsi",
					PciAddress: "0000:81:01.0",
				},
			},
		}
		apiDisk := &api.Disk{}
		devicePerBus := map[string]storage.DeviceNamer{}
		Expect(storage.Convert_v1_Disk_To_api_Disk(
			&convertertypes.ConverterContext{Architecture: archconverter.NewConverter(amd64)},
			disk, apiDisk, devicePerBus, nil, make(map[string]v1.VolumeStatus),
		)).ToNot(Succeed())
	})
})

func getDiskByName(domSpec api.DomainSpec, diskName string) (*api.Disk, error) {
	for i := range domSpec.Devices.Disks {
		disk := &domSpec.Devices.Disks[i]
		if disk.Alias.GetName() == diskName {
			return disk, nil
		}
	}
	return nil, fmt.Errorf("disk device '%s' not found", diskName)
}

func newEphemeralVolume(name string) v1.Volume {
	return v1.Volume{
		Name: name,
		VolumeSource: v1.VolumeSource{
			Ephemeral: &v1.EphemeralVolumeSource{
				PersistentVolumeClaim: &k8sv1.PersistentVolumeClaimVolumeSource{
					ClaimName: "testclaim",
				},
			},
		},
	}
}

var _ = Describe("ConvertDisks", func() {
	var ephemeralDiskCreator *fake.MockEphemeralDiskImageCreator

	BeforeEach(func() {
		ephemeralDiskCreator = &fake.MockEphemeralDiskImageCreator{BaseDir: "/var/run/libvirt/kubevirt-ephemeral-disk/"}
	})

	It("should generate the block backingstore disk within the domain", func() {
		blockPVCName := "pvc_block_test"
		vmi := libvmi.New(
			libvmi.WithEphemeralPersistentVolumeClaim(blockPVCName, "test-ephemeral"),
		)
		c := &convertertypes.ConverterContext{
			Architecture:         archconverter.NewConverter(runtime.GOARCH),
			EphemeraldiskCreator: ephemeralDiskCreator,
			IsBlockPVC:           map[string]bool{blockPVCName: true},
		}
		domain := &api.Domain{}
		Expect(storage.ConvertDisks(vmi, domain, c)).To(Succeed())
		Expect(domain.Spec.Devices.Disks[0].BackingStore).ToNot(BeNil())
		Expect(domain.Spec.Devices.Disks[0].BackingStore.Type).To(Equal("block"))
		Expect(domain.Spec.Devices.Disks[0].BackingStore.Source.Dev).To(Equal(storage.GetBlockDeviceVolumePath(blockPVCName)))
	})

	It("should succeed with SCSI reservation", func() {
		name := "scsi-reservation"
		vmi := &v1.VirtualMachineInstance{
			ObjectMeta: k8smeta.ObjectMeta{Name: "testvmi", Namespace: "default"},
			Spec: v1.VirtualMachineInstanceSpec{
				Domain: v1.DomainSpec{
					Devices: v1.Devices{
						Disks: []v1.Disk{{
							Name: name,
							DiskDevice: v1.DiskDevice{
								LUN: &v1.LunTarget{
									Bus:         "scsi",
									Reservation: true,
								},
							},
						}},
					},
				},
				Volumes: []v1.Volume{newEphemeralVolume(name)},
			},
		}
		c := &convertertypes.ConverterContext{
			Architecture:         archconverter.NewConverter(runtime.GOARCH),
			EphemeraldiskCreator: ephemeralDiskCreator,
		}
		domain := &api.Domain{}
		Expect(storage.ConvertDisks(vmi, domain, c)).To(Succeed())
		reserv := domain.Spec.Devices.Disks[0].Source.Reservations
		Expect(reserv.Managed).To(Equal("no"))
		Expect(reserv.SourceReservations.Type).To(Equal("unix"))
		Expect(reserv.SourceReservations.Path).To(Equal("/var/run/kubevirt/daemons/pr/pr-helper.sock"))
		Expect(reserv.SourceReservations.Mode).To(Equal("client"))
	})

	It("should allow CD-ROM with no volume", func() {
		name := "empty-cdrom"
		vmi := &v1.VirtualMachineInstance{
			ObjectMeta: k8smeta.ObjectMeta{Name: "testvmi", Namespace: "default"},
			Spec: v1.VirtualMachineInstanceSpec{
				Domain: v1.DomainSpec{
					Devices: v1.Devices{
						Disks: []v1.Disk{{
							Name: name,
							DiskDevice: v1.DiskDevice{
								CDRom: &v1.CDRomTarget{Bus: v1.DiskBusSATA},
							},
						}},
					},
				},
			},
		}
		c := &convertertypes.ConverterContext{
			Architecture: archconverter.NewConverter(runtime.GOARCH),
		}
		domain := &api.Domain{}
		Expect(storage.ConvertDisks(vmi, domain, c)).To(Succeed())
		Expect(domain.Spec.Devices.Disks).To(ContainElement(api.Disk{
			Type:     "block",
			Device:   "cdrom",
			Driver:   &api.DiskDriver{Name: "qemu", Type: "raw", ErrorPolicy: "stop", Discard: "unmap"},
			Target:   api.DiskTarget{Bus: "sata", Device: "sda"},
			ReadOnly: &api.ReadOnly{},
			Alias:    api.NewUserDefinedAlias(name),
		}))
	})

	Context("with CBT volumes", func() {
		DescribeTable("should create domain disk with datastore for filesystem volumes with CBT enabled",
			func(volumeName string, createVolumeSource func(string) v1.VolumeSource) {
				cbtPath := "/var/lib/libvirt/qemu/cbt/" + volumeName + ".qcow2"
				vmi := &v1.VirtualMachineInstance{
					ObjectMeta: k8smeta.ObjectMeta{Name: "testvmi", Namespace: "default"},
					Spec: v1.VirtualMachineInstanceSpec{
						Domain: v1.DomainSpec{
							Devices: v1.Devices{
								Disks: []v1.Disk{{
									Name: volumeName,
									DiskDevice: v1.DiskDevice{
										Disk: &v1.DiskTarget{Bus: v1.DiskBusVirtio},
									},
								}},
							},
						},
						Volumes: []v1.Volume{{
							Name:         volumeName,
							VolumeSource: createVolumeSource(volumeName),
						}},
					},
				}
				c := &convertertypes.ConverterContext{
					Architecture: archconverter.NewConverter(runtime.GOARCH),
					ApplyCBT:     map[string]string{volumeName: cbtPath},
				}
				domain := &api.Domain{}
				Expect(storage.ConvertDisks(vmi, domain, c)).To(Succeed())
				Expect(domain.Spec.Devices.Disks).To(HaveLen(1))
				disk := domain.Spec.Devices.Disks[0]
				Expect(disk.Type).To(Equal("file"))
				Expect(disk.Source.File).To(Equal(cbtPath))
				Expect(disk.Driver.Type).To(Equal("qcow2"))
				Expect(disk.Driver.ErrorPolicy).To(Equal(v1.DiskErrorPolicyStop))
				Expect(disk.Driver.Discard).To(Equal("unmap"))
				Expect(disk.Source.DataStore).ToNot(BeNil())
				Expect(disk.Source.DataStore.Type).To(Equal("file"))
				Expect(disk.Source.DataStore.Format).ToNot(BeNil())
				Expect(disk.Source.DataStore.Format.Type).To(Equal("raw"))
				Expect(disk.Source.DataStore.Source).ToNot(BeNil())
				Expect(disk.Source.DataStore.Source.File).ToNot(BeEmpty())
			},
			Entry("PVC", "test-pvc",
				func(name string) v1.VolumeSource {
					return v1.VolumeSource{
						PersistentVolumeClaim: &v1.PersistentVolumeClaimVolumeSource{
							PersistentVolumeClaimVolumeSource: k8sv1.PersistentVolumeClaimVolumeSource{ClaimName: name},
						},
					}
				},
			),
			Entry("DataVolume", "test-dv",
				func(name string) v1.VolumeSource {
					return v1.VolumeSource{
						DataVolume: &v1.DataVolumeSource{Name: name},
					}
				},
			),
			Entry("HostDisk", "test-hostdisk",
				func(name string) v1.VolumeSource {
					return v1.VolumeSource{
						HostDisk: &v1.HostDisk{
							Path: "/var/run/kubevirt-private/vmi-disks/" + name + "/disk.img",
							Type: v1.HostDiskExistsOrCreate,
						},
					}
				},
			),
		)

		DescribeTable("should create domain disk with datastore for block volumes with CBT enabled",
			func(volumeName string, createVolumeSource func(string) v1.VolumeSource, setupContext func(*convertertypes.ConverterContext, string)) {
				cbtPath := "/var/lib/libvirt/qemu/cbt/" + volumeName + ".qcow2"
				vmi := &v1.VirtualMachineInstance{
					ObjectMeta: k8smeta.ObjectMeta{Name: "testvmi", Namespace: "default"},
					Spec: v1.VirtualMachineInstanceSpec{
						Domain: v1.DomainSpec{
							Devices: v1.Devices{
								Disks: []v1.Disk{{
									Name: volumeName,
									DiskDevice: v1.DiskDevice{
										Disk: &v1.DiskTarget{Bus: v1.DiskBusVirtio},
									},
								}},
							},
						},
						Volumes: []v1.Volume{{
							Name:         volumeName,
							VolumeSource: createVolumeSource(volumeName),
						}},
					},
				}
				c := &convertertypes.ConverterContext{
					Architecture: archconverter.NewConverter(runtime.GOARCH),
					ApplyCBT:     map[string]string{volumeName: cbtPath},
				}
				setupContext(c, volumeName)
				domain := &api.Domain{}
				Expect(storage.ConvertDisks(vmi, domain, c)).To(Succeed())
				Expect(domain.Spec.Devices.Disks).To(HaveLen(1))
				disk := domain.Spec.Devices.Disks[0]
				Expect(disk.Type).To(Equal("file"))
				Expect(disk.Source.File).To(Equal(cbtPath))
				Expect(disk.Source.Name).To(Equal(volumeName))
				Expect(disk.Driver.Type).To(Equal("qcow2"))
				Expect(disk.Driver.ErrorPolicy).To(Equal(v1.DiskErrorPolicyStop))
				Expect(disk.Driver.Discard).To(Equal("unmap"))
				Expect(disk.Source.DataStore).ToNot(BeNil())
				Expect(disk.Source.DataStore.Type).To(Equal("block"))
				Expect(disk.Source.DataStore.Format).ToNot(BeNil())
				Expect(disk.Source.DataStore.Format.Type).To(Equal("raw"))
				Expect(disk.Source.DataStore.Source).ToNot(BeNil())
				Expect(disk.Source.DataStore.Source.Dev).To(Equal(storage.GetBlockDeviceVolumePath(volumeName)))
			},
			Entry("PVC", "test-block-pvc",
				func(name string) v1.VolumeSource {
					return v1.VolumeSource{
						PersistentVolumeClaim: &v1.PersistentVolumeClaimVolumeSource{
							PersistentVolumeClaimVolumeSource: k8sv1.PersistentVolumeClaimVolumeSource{ClaimName: name},
						},
					}
				},
				func(c *convertertypes.ConverterContext, name string) {
					c.IsBlockPVC = map[string]bool{name: true}
				},
			),
			Entry("DataVolume", "test-block-dv",
				func(name string) v1.VolumeSource {
					return v1.VolumeSource{
						DataVolume: &v1.DataVolumeSource{Name: name},
					}
				},
				func(c *convertertypes.ConverterContext, name string) {
					c.IsBlockDV = map[string]bool{name: true}
				},
			),
		)
	})

	It("should not overwrite the IO policy when IO threads are enabled", func() {
		ioPolicy := v1.IONative
		vmi := &v1.VirtualMachineInstance{
			ObjectMeta: k8smeta.ObjectMeta{Name: "testvmi", Namespace: "default"},
			Spec: v1.VirtualMachineInstanceSpec{
				Domain: v1.DomainSpec{
					Devices: v1.Devices{
						Disks: []v1.Disk{{
							Name:              "disk",
							DedicatedIOThread: pointer.P(true),
							IO:                ioPolicy,
						}},
					},
				},
				Volumes: []v1.Volume{newEphemeralVolume("disk")},
			},
		}
		c := &convertertypes.ConverterContext{
			Architecture:         archconverter.NewConverter(runtime.GOARCH),
			EphemeraldiskCreator: ephemeralDiskCreator,
		}
		domain := &api.Domain{}
		Expect(storage.ConvertDisks(vmi, domain, c)).To(Succeed())
		Expect(domain.Spec.Devices.Disks[0].Driver.IO).To(Equal(ioPolicy))
	})

	DescribeTable("Should set the error policy", func(epolicy *v1.DiskErrorPolicy, expected string) {
		vmi := &v1.VirtualMachineInstance{
			ObjectMeta: k8smeta.ObjectMeta{Name: "testvmi", Namespace: "default"},
			Spec: v1.VirtualMachineInstanceSpec{
				Domain: v1.DomainSpec{
					Devices: v1.Devices{
						Disks: []v1.Disk{{
							Name: "mydisk",
							DiskDevice: v1.DiskDevice{
								Disk: &v1.DiskTarget{Bus: v1.VirtIO},
							},
							ErrorPolicy: epolicy,
						}},
					},
				},
				Volumes: []v1.Volume{{
					Name: "mydisk",
					VolumeSource: v1.VolumeSource{
						PersistentVolumeClaim: &v1.PersistentVolumeClaimVolumeSource{
							PersistentVolumeClaimVolumeSource: k8sv1.PersistentVolumeClaimVolumeSource{ClaimName: "testclaim"},
						},
					},
				}},
			},
		}
		c := &convertertypes.ConverterContext{
			Architecture: archconverter.NewConverter(runtime.GOARCH),
		}
		domain := &api.Domain{}
		Expect(storage.ConvertDisks(vmi, domain, c)).To(Succeed())
		Expect(string(domain.Spec.Devices.Disks[0].Driver.ErrorPolicy)).To(Equal(expected))
	},
		Entry("ErrorPolicy not specified", nil, "stop"),
		Entry("ErrorPolicy equal to stop", pointer.P(v1.DiskErrorPolicyStop), "stop"),
		Entry("ErrorPolicy equal to ignore", pointer.P(v1.DiskErrorPolicyIgnore), "ignore"),
		Entry("ErrorPolicy equal to report", pointer.P(v1.DiskErrorPolicyReport), "report"),
		Entry("ErrorPolicy equal to enospace", pointer.P(v1.DiskErrorPolicyEnospace), "enospace"),
	)

	Context("IOThreads", func() {
		DescribeTable("Should use correct IOThreads policies", func(policy v1.IOThreadsPolicy, cpuCores int, threadIDs []int) {
			vmi := &v1.VirtualMachineInstance{
				ObjectMeta: k8smeta.ObjectMeta{
					Name:      "testvmi",
					Namespace: "default",
					UID:       "1234",
				},
				Spec: v1.VirtualMachineInstanceSpec{
					Domain: v1.DomainSpec{
						IOThreadsPolicy: &policy,
						Resources: v1.ResourceRequirements{
							Requests: k8sv1.ResourceList{
								k8sv1.ResourceCPU: resource.MustParse(fmt.Sprintf("%d", cpuCores)),
							},
						},
						Devices: v1.Devices{
							Disks: []v1.Disk{
								{Name: "dedicated", DiskDevice: v1.DiskDevice{Disk: &v1.DiskTarget{Bus: v1.VirtIO}}, DedicatedIOThread: pointer.P(true)},
								{Name: "shared", DiskDevice: v1.DiskDevice{Disk: &v1.DiskTarget{Bus: v1.VirtIO}}, DedicatedIOThread: pointer.P(false)},
								{Name: "omitted1", DiskDevice: v1.DiskDevice{Disk: &v1.DiskTarget{Bus: v1.VirtIO}}},
								{Name: "omitted2", DiskDevice: v1.DiskDevice{Disk: &v1.DiskTarget{Bus: v1.VirtIO}}},
								{Name: "omitted3", DiskDevice: v1.DiskDevice{Disk: &v1.DiskTarget{Bus: v1.VirtIO}}},
								{Name: "omitted4", DiskDevice: v1.DiskDevice{Disk: &v1.DiskTarget{Bus: v1.VirtIO}}},
								{Name: "omitted5", DiskDevice: v1.DiskDevice{Disk: &v1.DiskTarget{Bus: v1.VirtIO}}},
							},
						},
					},
					Volumes: []v1.Volume{
						newEphemeralVolume("dedicated"),
						newEphemeralVolume("shared"),
						newEphemeralVolume("omitted1"),
						newEphemeralVolume("omitted2"),
						newEphemeralVolume("omitted3"),
						newEphemeralVolume("omitted4"),
						newEphemeralVolume("omitted5"),
					},
				},
			}
			c := &convertertypes.ConverterContext{
				Architecture:         archconverter.NewConverter(runtime.GOARCH),
				EphemeraldiskCreator: ephemeralDiskCreator,
			}
			domain := &api.Domain{}
			Expect(storage.ConvertDisks(vmi, domain, c)).To(Succeed())
			for idx, disk := range domain.Spec.Devices.Disks {
				Expect(disk.Driver.IOThread).ToNot(BeNil())
				Expect(int(*disk.Driver.IOThread)).To(Equal(threadIDs[idx]))
			}
		},
			Entry("using a shared policy with 1 CPU", v1.IOThreadsPolicyShared, 1, []int{2, 1, 1, 1, 1, 1, 1}),
			Entry("using a shared policy with 2 CPUs", v1.IOThreadsPolicyShared, 2, []int{2, 1, 1, 1, 1, 1, 1}),
			Entry("using a shared policy with 3 CPUs", v1.IOThreadsPolicyShared, 2, []int{2, 1, 1, 1, 1, 1, 1}),
			Entry("using an auto policy with 1 CPU", v1.IOThreadsPolicyAuto, 1, []int{2, 1, 1, 1, 1, 1, 1}),
			Entry("using an auto policy with 2 CPUs", v1.IOThreadsPolicyAuto, 2, []int{4, 1, 2, 3, 1, 2, 3}),
			Entry("using an auto policy with 3 CPUs", v1.IOThreadsPolicyAuto, 3, []int{6, 1, 2, 3, 4, 5, 1}),
			Entry("using an auto policy with 4 CPUs", v1.IOThreadsPolicyAuto, 4, []int{7, 1, 2, 3, 4, 5, 6}),
			Entry("using an auto policy with 5 CPUs", v1.IOThreadsPolicyAuto, 5, []int{7, 1, 2, 3, 4, 5, 6}),
		)

		It("Should not add IOThreads to non-virtio disks", func() {
			vmi := &v1.VirtualMachineInstance{
				ObjectMeta: k8smeta.ObjectMeta{Name: "testvmi", Namespace: "default", UID: "1234"},
				Spec: v1.VirtualMachineInstanceSpec{
					Domain: v1.DomainSpec{
						Devices: v1.Devices{
							Disks: []v1.Disk{
								{Name: "dedicated", DiskDevice: v1.DiskDevice{Disk: &v1.DiskTarget{Bus: v1.VirtIO}}, DedicatedIOThread: pointer.P(true)},
								{Name: "shared", DiskDevice: v1.DiskDevice{Disk: &v1.DiskTarget{Bus: v1.VirtIO}}, DedicatedIOThread: pointer.P(false)},
								{Name: "incompatible", DiskDevice: v1.DiskDevice{Disk: &v1.DiskTarget{Bus: v1.DiskBusSATA}}},
							},
						},
					},
					Volumes: []v1.Volume{
						newEphemeralVolume("dedicated"),
						newEphemeralVolume("shared"),
						newEphemeralVolume("incompatible"),
					},
				},
			}
			c := &convertertypes.ConverterContext{
				Architecture:         archconverter.NewConverter(runtime.GOARCH),
				EphemeraldiskCreator: ephemeralDiskCreator,
			}
			domain := &api.Domain{}
			Expect(storage.ConvertDisks(vmi, domain, c)).To(Succeed())
			Expect(domain.Spec.Devices.Disks[0].Driver.IOThread).ToNot(BeNil())
			Expect(*domain.Spec.Devices.Disks[0].Driver.IOThread).To(Equal(uint(2)))
			Expect(domain.Spec.Devices.Disks[1].Driver.IOThread).ToNot(BeNil())
			Expect(*domain.Spec.Devices.Disks[1].Driver.IOThread).To(Equal(uint(1)))
			Expect(domain.Spec.Devices.Disks[2].Driver.IOThread).To(BeNil())
		})

		It("Should set the iothread pool with the supplementalPool policy", func() {
			count := uint32(4)
			vmi := libvmi.New(
				libvmi.WithIOThreadsPolicy(v1.IOThreadsPolicySupplementalPool),
				libvmi.WithIOThreads(v1.DiskIOThreads{SupplementalPoolThreadCount: pointer.P(count)}),
				libvmi.WithPersistentVolumeClaim("disk0", "pvc0", libvmi.WithDedicatedIOThreads(true)),
			)
			expectedIOThreads := &api.DiskIOThreads{}
			for id := 1; id <= int(count); id++ {
				expectedIOThreads.IOThread = append(expectedIOThreads.IOThread, api.DiskIOThread{Id: uint32(id)})
			}
			c := &convertertypes.ConverterContext{
				Architecture:         archconverter.NewConverter(runtime.GOARCH),
				EphemeraldiskCreator: ephemeralDiskCreator,
			}
			domain := &api.Domain{}
			Expect(storage.ConvertDisks(vmi, domain, c)).To(Succeed())
			Expect(domain.Spec.Devices.Disks[0].Driver.IOThreads).To(Equal(expectedIOThreads))
		})

		It("Should honor shared ioThreadsPolicy for single disk", func() {
			vmi := libvmi.New(
				libvmi.WithIOThreadsPolicy(v1.IOThreadsPolicyShared),
				libvmi.WithPersistentVolumeClaim("disk0", "alpine"),
			)
			c := &convertertypes.ConverterContext{
				Architecture:         archconverter.NewConverter(runtime.GOARCH),
				EphemeraldiskCreator: ephemeralDiskCreator,
			}
			domain := &api.Domain{}
			Expect(storage.ConvertDisks(vmi, domain, c)).To(Succeed())
			Expect(domain.Spec.Devices.Disks).To(HaveLen(1))
			Expect(domain.Spec.Devices.Disks[0].Driver.IOThread).ToNot(BeNil())
		})

		It("Should honor a mix of shared and dedicated ioThreadsPolicy", func() {
			vmi := libvmi.New(
				libvmi.WithIOThreadsPolicy(v1.IOThreadsPolicyShared),
				libvmi.WithPersistentVolumeClaim("disk0", "alpine", libvmi.WithDedicatedIOThreads(true)),
				libvmi.WithPersistentVolumeClaim("shr1", "alpine"),
				libvmi.WithPersistentVolumeClaim("shr2", "alpine"),
			)
			c := &convertertypes.ConverterContext{
				Architecture:         archconverter.NewConverter(runtime.GOARCH),
				EphemeraldiskCreator: ephemeralDiskCreator,
			}
			domain := &api.Domain{}
			Expect(storage.ConvertDisks(vmi, domain, c)).To(Succeed())
			disk0, err := getDiskByName(domain.Spec, "disk0")
			Expect(err).ToNot(HaveOccurred())
			disk1, err := getDiskByName(domain.Spec, "shr1")
			Expect(err).ToNot(HaveOccurred())
			disk2, err := getDiskByName(domain.Spec, "shr2")
			Expect(err).ToNot(HaveOccurred())
			Expect(*disk1.Driver.IOThread).To(Equal(*disk2.Driver.IOThread))
			Expect(*disk0.Driver.IOThread).ToNot(Equal(*disk1.Driver.IOThread))
		})

		DescribeTable("should honor auto ioThreadPolicy", func(numCpus int, expectedIOThreads int) {
			vmi := libvmi.New(
				libvmi.WithIOThreadsPolicy(v1.IOThreadsPolicyAuto),
				libvmi.WithCPURequest(strconv.Itoa(numCpus)),
				libvmi.WithPersistentVolumeClaim("disk0", "alpine", libvmi.WithDedicatedIOThreads(true)),
				libvmi.WithPersistentVolumeClaim("ded2", "alpine", libvmi.WithDedicatedIOThreads(true)),
				libvmi.WithPersistentVolumeClaim("shr1", "alpine"),
				libvmi.WithPersistentVolumeClaim("shr2", "alpine"),
				libvmi.WithPersistentVolumeClaim("shr3", "alpine"),
				libvmi.WithPersistentVolumeClaim("shr4", "alpine"),
			)
			c := &convertertypes.ConverterContext{
				Architecture:         archconverter.NewConverter(runtime.GOARCH),
				EphemeraldiskCreator: ephemeralDiskCreator,
			}
			domain := &api.Domain{}
			Expect(storage.ConvertDisks(vmi, domain, c)).To(Succeed())
			disk0, err := getDiskByName(domain.Spec, "disk0")
			Expect(err).ToNot(HaveOccurred())
			ded2, err := getDiskByName(domain.Spec, "ded2")
			Expect(err).ToNot(HaveOccurred())
			shr1, err := getDiskByName(domain.Spec, "shr1")
			Expect(err).ToNot(HaveOccurred())
			shr2, err := getDiskByName(domain.Spec, "shr2")
			Expect(err).ToNot(HaveOccurred())
			shr3, err := getDiskByName(domain.Spec, "shr3")
			Expect(err).ToNot(HaveOccurred())
			shr4, err := getDiskByName(domain.Spec, "shr4")
			Expect(err).ToNot(HaveOccurred())
			Expect(*disk0.Driver.IOThread).ToNot(Equal(*ded2.Driver.IOThread), "disk0 should have a dedicated ioThread")
			Expect(*disk0.Driver.IOThread).ToNot(Equal(*shr1.Driver.IOThread), "disk0 should have a dedicated ioThread")
			Expect(*disk0.Driver.IOThread).ToNot(Equal(*shr2.Driver.IOThread), "disk0 should have a dedicated ioThread")
			Expect(*disk0.Driver.IOThread).ToNot(Equal(*shr3.Driver.IOThread), "disk0 should have a dedicated ioThread")
			Expect(*disk0.Driver.IOThread).ToNot(Equal(*shr4.Driver.IOThread), "disk0 should have a dedicated ioThread")
			Expect(*ded2.Driver.IOThread).ToNot(Equal(*shr1.Driver.IOThread), "ded2 should have a dedicated ioThread")
			Expect(*ded2.Driver.IOThread).ToNot(Equal(*shr2.Driver.IOThread), "ded2 should have a dedicated ioThread")
			Expect(*ded2.Driver.IOThread).ToNot(Equal(*shr3.Driver.IOThread), "ded2 should have a dedicated ioThread")
			Expect(*ded2.Driver.IOThread).ToNot(Equal(*shr4.Driver.IOThread), "ded2 should have a dedicated ioThread")
		},
			Entry("for one CPU", 1, 3),
			Entry("for two CPUs", 2, 4),
			Entry("for three CPUs", 3, 6),
			Entry("for four CPUs", 4, 6),
		)
	})

	Context("virtio block multi-queue", func() {
		var vmi *v1.VirtualMachineInstance

		BeforeEach(func() {
			vmi = &v1.VirtualMachineInstance{
				ObjectMeta: k8smeta.ObjectMeta{Name: "testvmi", Namespace: "mynamespace"},
			}
			v1.SetObjectDefaults_VirtualMachineInstance(vmi)
			vmi.Spec.Domain.Devices.Disks = []v1.Disk{{
				Name: "mydisk",
				DiskDevice: v1.DiskDevice{
					Disk: &v1.DiskTarget{Bus: v1.VirtIO},
				},
			}}
			vmi.Spec.Volumes = []v1.Volume{{
				Name: "mydisk",
				VolumeSource: v1.VolumeSource{
					HostDisk: &v1.HostDisk{
						Path:     "/var/run/kubevirt-private/vmi-disks/myvolume/disk.img",
						Type:     v1.HostDiskExistsOrCreate,
						Capacity: resource.MustParse("1Gi"),
					},
				},
			}}
			vmi.Spec.Domain.Devices.BlockMultiQueue = pointer.P(true)
			vmi.Spec.Domain.Resources.Requests = k8sv1.ResourceList{
				k8sv1.ResourceMemory: resource.MustParse("8192Ki"),
				k8sv1.ResourceCPU:    resource.MustParse("2"),
			}
		})

		It("should assign correct number of queues with CPU hotplug topology", func() {
			vmi.Spec.Domain.Resources.Requests = k8sv1.ResourceList{}
			vmi.Spec.Domain.CPU = &v1.CPU{
				Cores:      2,
				Threads:    2,
				Sockets:    2,
				MaxSockets: 8,
			}
			expectedNumOfBlkQueues :=
				vmi.Spec.Domain.CPU.Cores * vmi.Spec.Domain.CPU.Threads * vmi.Spec.Domain.CPU.Sockets
			c := &convertertypes.ConverterContext{Architecture: archconverter.NewConverter(runtime.GOARCH)}
			domain := &api.Domain{}
			Expect(storage.ConvertDisks(vmi, domain, c)).To(Succeed())
			Expect(domain.Spec.Devices.Disks).To(HaveLen(1))
			disk := domain.Spec.Devices.Disks[0]
			Expect(disk.Driver.Queues).ToNot(BeNil())
			Expect(*disk.Driver.Queues).To(Equal(uint(expectedNumOfBlkQueues)))
		})

		It("should honor multiQueue setting", func() {
			var expectedQueues uint = 2
			vmi.Spec.Domain.CPU = &v1.CPU{Cores: 2}
			c := &convertertypes.ConverterContext{Architecture: archconverter.NewConverter(runtime.GOARCH)}
			domain := &api.Domain{}
			Expect(storage.ConvertDisks(vmi, domain, c)).To(Succeed())
			Expect(*(domain.Spec.Devices.Disks[0].Driver.Queues)).To(Equal(expectedQueues),
				"expected number of queues to equal number of requested vCPUs")
		})
	})

	Context("hotplug", func() {
		var c *convertertypes.ConverterContext

		type ConverterFunc = func(name string, disk *api.Disk, c *convertertypes.ConverterContext) error

		BeforeEach(func() {
			c = &convertertypes.ConverterContext{
				Architecture:         archconverter.NewConverter(runtime.GOARCH),
				IsBlockPVC:           map[string]bool{"test-block-pvc": true},
				IsBlockDV:            map[string]bool{"test-block-dv": true},
				VolumesDiscardIgnore: []string{"test-discard-ignore"},
			}
		})

		DescribeTable("should convert",
			func(converterFunc ConverterFunc, volumeName string, isBlockMode bool, ignoreDiscard bool) {
				expectedDisk := &api.Disk{}
				expectedDisk.Driver = &api.DiskDriver{}
				expectedDisk.Driver.Type = "raw"
				expectedDisk.Driver.ErrorPolicy = "stop"
				if isBlockMode {
					expectedDisk.Type = "block"
					expectedDisk.Source.Dev = filepath.Join(v1.HotplugDiskDir, volumeName)
				} else {
					expectedDisk.Type = "file"
					expectedDisk.Source.File = fmt.Sprintf("%s.img", filepath.Join(v1.HotplugDiskDir, volumeName))
				}
				if !ignoreDiscard {
					expectedDisk.Driver.Discard = "unmap"
				}
				disk := &api.Disk{Driver: &api.DiskDriver{}}
				Expect(converterFunc(volumeName, disk, c)).To(Succeed())
				Expect(disk).To(Equal(expectedDisk))
			},
			Entry("filesystem PVC", storage.Convert_v1_Hotplug_PersistentVolumeClaim_To_api_Disk, "test-fs-pvc", false, false),
			Entry("block mode PVC", storage.Convert_v1_Hotplug_PersistentVolumeClaim_To_api_Disk, "test-block-pvc", true, false),
			Entry("'discard ignore' PVC", storage.Convert_v1_Hotplug_PersistentVolumeClaim_To_api_Disk, "test-discard-ignore", false, true),
			Entry("filesystem DV", storage.Convert_v1_Hotplug_DataVolume_To_api_Disk, "test-fs-dv", false, false),
			Entry("block mode DV", storage.Convert_v1_Hotplug_DataVolume_To_api_Disk, "test-block-dv", true, false),
			Entry("'discard ignore' DV", storage.Convert_v1_Hotplug_DataVolume_To_api_Disk, "test-discard-ignore", false, true),
		)

		DescribeTable("should create domain disk with datastore for hotplug volumes with CBT enabled",
			func(volumeName string, volSource v1.VolumeSource, isBlock bool) {
				cbtPath := "/var/lib/libvirt/qemu/cbt/" + volumeName + ".qcow2"
				vmi := &v1.VirtualMachineInstance{
					ObjectMeta: k8smeta.ObjectMeta{Name: "testvmi", Namespace: "mynamespace"},
				}
				v1.SetObjectDefaults_VirtualMachineInstance(vmi)
				vmi.Spec.Domain.Devices.Disks = []v1.Disk{{
					Name: volumeName,
					DiskDevice: v1.DiskDevice{
						Disk: &v1.DiskTarget{Bus: v1.DiskBusVirtio},
					},
				}}
				vmi.Spec.Volumes = []v1.Volume{{
					Name:         volumeName,
					VolumeSource: volSource,
				}}
				c.ApplyCBT = map[string]string{volumeName: cbtPath}
				c.HotplugVolumes = map[string]v1.VolumeStatus{
					volumeName: {Name: volumeName, Phase: v1.HotplugVolumeMounted, HotplugVolume: &v1.HotplugVolumeStatus{}},
				}
				if isBlock {
					if volSource.PersistentVolumeClaim != nil {
						c.IsBlockPVC[volumeName] = true
					} else if volSource.DataVolume != nil {
						c.IsBlockDV[volumeName] = true
					}
				}
				domain := &api.Domain{}
				Expect(storage.ConvertDisks(vmi, domain, c)).To(Succeed())
				Expect(domain.Spec.Devices.Disks).To(HaveLen(1))
				disk := domain.Spec.Devices.Disks[0]
				Expect(disk.Type).To(Equal("file"))
				Expect(disk.Source.File).To(Equal(cbtPath))
				Expect(disk.Driver.Type).To(Equal("qcow2"))
				Expect(disk.Driver.ErrorPolicy).To(Equal(v1.DiskErrorPolicyStop))
				Expect(disk.Driver.Discard).To(Equal("unmap"))
				Expect(disk.Source.DataStore).ToNot(BeNil())
				Expect(disk.Source.DataStore.Format).ToNot(BeNil())
				Expect(disk.Source.DataStore.Format.Type).To(Equal("raw"))
				Expect(disk.Source.DataStore.Source).ToNot(BeNil())
				if isBlock {
					Expect(disk.Source.DataStore.Type).To(Equal("block"))
					Expect(disk.Source.DataStore.Source.Dev).To(Equal(storage.GetHotplugBlockDeviceVolumePath(volumeName)))
				} else {
					Expect(disk.Source.DataStore.Type).To(Equal("file"))
					Expect(disk.Source.DataStore.Source.File).To(Equal(storage.GetHotplugFilesystemVolumePath(volumeName)))
				}
			},
			Entry("filesystem PVC", "test-hotplug-pvc",
				v1.VolumeSource{PersistentVolumeClaim: &v1.PersistentVolumeClaimVolumeSource{
					PersistentVolumeClaimVolumeSource: k8sv1.PersistentVolumeClaimVolumeSource{ClaimName: "test-hotplug-pvc"},
					Hotpluggable:                      true,
				}}, false),
			Entry("filesystem DataVolume", "test-hotplug-dv",
				v1.VolumeSource{DataVolume: &v1.DataVolumeSource{Name: "test-hotplug-dv", Hotpluggable: true}}, false),
			Entry("block PVC", "test-hotplug-block-pvc",
				v1.VolumeSource{PersistentVolumeClaim: &v1.PersistentVolumeClaimVolumeSource{
					PersistentVolumeClaimVolumeSource: k8sv1.PersistentVolumeClaimVolumeSource{ClaimName: "test-hotplug-block-pvc"},
					Hotpluggable:                      true,
				}}, true),
			Entry("block DataVolume", "test-hotplug-block-dv",
				v1.VolumeSource{DataVolume: &v1.DataVolumeSource{Name: "test-hotplug-block-dv", Hotpluggable: true}}, true),
		)
	})
})
