//go:build amd64 || s390x

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
 * Copyright 2021 Red Hat, Inc.
 *
 */

package nodecapabilities

import (
	_ "embed"
	"runtime"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"libvirt.org/go/libvirtxml"

	"kubevirt.io/kubevirt/pkg/pointer"
)

//go:embed testdata/virsh_domcapabilities.xml
var domainCapabilitiesXML string

//go:embed testdata/virsh_domcapabilities_nothing_usable.xml
var domainCapabilitiesNothingUsableXML string

//go:embed testdata/domcapabilities_sev.xml
var domainCapabilitiesSevXML string

//go:embed testdata/domcapabilities_nosev.xml
var domainCapabilitiesNoSevXML string

//go:embed testdata/domcapabilities_seves.xml
var domainCapabilitiesSevESXML string

//go:embed testdata/s390x/virsh_domcapabilities.xml
var s390xDomainCapabilitiesXML string

//go:embed testdata/supported_features.xml
var supportedFeaturesXML string

//go:embed testdata/s390x/supported_features.xml
var s390xSupportedFeaturesXML string

//go:embed testdata/capabilities.xml
var hostCapabilitiesXML string

//go:embed testdata/capabilities_with_numa.xml
var hostCapablitiesWithNUMAXML string

var _ = Describe("node-capabilities", func() {

	Describe("Host capabilities", func() {
		It("should be able to read the TSC timer frequency from the host", func() {
			capabilities, err := HostCapabilities(hostCapabilitiesXML)
			Expect(err).ToNot(HaveOccurred())
			Expect(capabilities.Host.CPU.Counter.Name).To(Equal("tsc"))
			Expect(capabilities.Host.CPU.Counter.Frequency).To(BeNumerically("==", 4008012000))
			Expect(capabilities.Host.CPU.Counter.Scaling == "yes").To(BeFalse())
		})

		It("should properly read cpu siblings", func() {
			capabilities, err := HostCapabilities(hostCapabilitiesXML)
			Expect(err).ToNot(HaveOccurred())
			Expect(capabilities.Host.NUMA.Cells.Cells).To(HaveLen(1))
			Expect(capabilities.Host.NUMA.Cells.Cells[0].CPUS.CPUs).To(HaveLen(8))
			Expect(capabilities.Host.NUMA.Cells.Cells[0].CPUS.CPUs[0].Siblings).To(Equal("0,4"))
		})

		It("should read the NUMA topology from the host", func() {
			expectedCell := libvirtxml.CapsHostNUMACell{
				ID:     0,
				Memory: &libvirtxml.CapsHostNUMAMemory{Size: 1289144, Unit: "KiB"},
				PageInfo: []libvirtxml.CapsHostNUMAPageInfo{
					{Count: 314094, Unit: "KiB", Size: 4},
					{Count: 16, Unit: "KiB", Size: 2048},
					{Count: 0, Unit: "KiB", Size: 1048576},
				},
				Distances: &libvirtxml.CapsHostNUMADistances{
					Siblings: []libvirtxml.CapsHostNUMASibling{
						{ID: 0, Value: 10},
						{ID: 1, Value: 10},
						{ID: 2, Value: 10},
						{ID: 3, Value: 10},
					},
				},
				CPUS: &libvirtxml.CapsHostNUMACPUs{
					Num: 6,
					CPUs: []libvirtxml.CapsHostNUMACPU{
						{ID: 0, SocketID: pointer.P(0), DieID: pointer.P(0), CoreID: pointer.P(0), Siblings: "0"},
						{ID: 1, SocketID: pointer.P(1), DieID: pointer.P(0), CoreID: pointer.P(0), Siblings: "1"},
						{ID: 2, SocketID: pointer.P(2), DieID: pointer.P(0), CoreID: pointer.P(0), Siblings: "2"},
						{ID: 3, SocketID: pointer.P(3), DieID: pointer.P(0), CoreID: pointer.P(0), Siblings: "3"},
						{ID: 4, SocketID: pointer.P(4), DieID: pointer.P(0), CoreID: pointer.P(0), Siblings: "4"},
						{ID: 5, SocketID: pointer.P(5), DieID: pointer.P(0), CoreID: pointer.P(0), Siblings: "5"},
					},
				},
			}

			capabilities, err := HostCapabilities(hostCapablitiesWithNUMAXML)
			Expect(err).ToNot(HaveOccurred())
			Expect(capabilities.Host.NUMA.Cells.Num).To(BeNumerically("==", 4))
			Expect(capabilities.Host.NUMA.Cells.Cells).To(HaveLen(4))
			Expect(capabilities.Host.NUMA.Cells.Cells[0]).To(Equal(expectedCell))
		})
	})

	Describe("Domain capabilities", func() {
		It("should return correct cpu models", func() {
			domainCapabilities, err := DomCapabilities(domainCapabilitiesXML)
			Expect(err).ToNot(HaveOccurred())

			cpuFeatures, err := SupportedFeatures(supportedFeaturesXML, runtime.GOARCH)
			Expect(err).ToNot(HaveOccurred())

			supportedCPUs, err := SupportedHostCPUs(domainCapabilities.CPU.Modes, runtime.GOARCH)
			Expect(err).ToNot(HaveOccurred())

			Expect(supportedCPUs.UsableModels).To(HaveLen(5), "number of models must match")
			Expect(cpuFeatures).To(HaveLen(4), "number of features must match")
		})

		It("No cpu model is usable", func() {
			domainCapabilities, err := DomCapabilities(domainCapabilitiesNothingUsableXML)
			Expect(err).ToNot(HaveOccurred())

			supportedCPUs, err := SupportedHostCPUs(domainCapabilities.CPU.Modes, runtime.GOARCH)
			Expect(err).ToNot(HaveOccurred())

			cpuFeatures, err := SupportedFeatures(supportedFeaturesXML, runtime.GOARCH)
			Expect(err).ToNot(HaveOccurred())

			Expect(supportedCPUs.UsableModels).To(BeEmpty(), "no CPU models are expected to be supported")
			Expect(cpuFeatures).To(HaveLen(4), "number of features must match")
		})

		It("Should return the cpu features on s390x even without policy='require' property", func() {
			cpuFeatures, err := SupportedFeatures(s390xSupportedFeaturesXML, "s390x")
			Expect(err).ToNot(HaveOccurred())

			Expect(cpuFeatures).To(HaveLen(89), "number of features doesn't match")
		})

		It("Should return the cpu features on amd64 only with policy='require' property", func() {
			cpuFeatures, err := SupportedFeatures(s390xSupportedFeaturesXML, "amd64")
			Expect(err).ToNot(HaveOccurred())

			Expect(cpuFeatures).To(BeEmpty(), "number of features doesn't match")
		})

		It("Should default to IBM as CPU Vendor on s390x if none is given", func() {
			domainCapabilities, err := DomCapabilities(s390xDomainCapabilitiesXML)
			Expect(err).ToNot(HaveOccurred())

			supportedCPUs, err := SupportedHostCPUs(domainCapabilities.CPU.Modes, "s390x")

			Expect(supportedCPUs.Vendor).To(Equal("IBM"), "CPU Vendor should be IBM")
		})

		Context("should return correct host cpu", func() {
			var supportedCPUs *SupportedCPU
			BeforeEach(func() {
				domainCapabilities, err := DomCapabilities(domainCapabilitiesXML)
				Expect(err).ToNot(HaveOccurred())

				supportedCPUs, err = SupportedHostCPUs(domainCapabilities.CPU.Modes, runtime.GOARCH)
				Expect(err).ToNot(HaveOccurred())
			})

			It("model", func() {
				Expect(supportedCPUs.Model).To(Equal("Skylake-Client-IBRS"))
			})

			It("required features", func() {
				Expect(supportedCPUs.RequiredFeatures).To(HaveLen(3))
				Expect(supportedCPUs.RequiredFeatures).Should(ConsistOf(
					"ds",
					"acpi",
					"ss",
				))
			})
		})

		Context("return correct SEV capabilities", func() {
			DescribeTable("for SEV and SEV-ES", func(domCapabilitiesXML string) {
				domCapabilities, err := DomCapabilities(domCapabilitiesXML)
				Expect(err).ToNot(HaveOccurred())

				sev := domCapabilities.Features.SEV
				supportedSev := SupportedHostSEV(sev)

				if supportedSev.Supported {
					Expect(sev.Supported).To(Equal("yes"))
					Expect(sev.CBitPos).To(Equal(uint(47)))
					Expect(sev.ReducedPhysBits).To(Equal(uint(1)))
					Expect(sev.MaxGuests).To(Equal(uint(15)))

					if supportedSev.SupportedES {
						Expect(sev.MaxESGuests).To(Equal(uint(15)))
					} else {
						Expect(sev.MaxESGuests).To(BeZero())
					}
				} else {
					Expect(sev.Supported).To(Equal("no"))
					Expect(sev.CBitPos).To(BeZero())
					Expect(sev.ReducedPhysBits).To(BeZero())
					Expect(sev.MaxGuests).To(BeZero())
					Expect(sev.MaxESGuests).To(BeZero())
				}
			},
				Entry("when only SEV is supported", domainCapabilitiesSevXML),
				Entry("when both SEV and SEV-ES are supported", domainCapabilitiesSevESXML),
				Entry("when neither SEV nor SEV-ES are supported", domainCapabilitiesNoSevXML),
			)
		})
	})
})
