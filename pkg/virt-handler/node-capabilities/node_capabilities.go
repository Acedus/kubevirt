/*
Copyright 2024 The KubeVirt Authors.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package nodecapabilities

import (
	"fmt"
	"io"
	"runtime"

	"kubevirt.io/client-go/log"
	"libvirt.org/go/libvirtxml"

	virtconfig "kubevirt.io/kubevirt/pkg/virt-config"
)

const (
	CapabilitiesVolumePath    = "/var/lib/kubevirt-node-capabilities/"
	HostCapabilitiesFilename  = "capabilities.xml"
	DomCapabiliitesFilename   = "virsh_domcapabilities.xml"
	SupportedFeaturesFilename = "supported_features.xml"
)

type SupportedCPU struct {
	Vendor           string
	Model            libvirtxml.DomainCapsCPUModel
	RequiredFeatures []libvirtxml.DomainCapsCPUFeature
	UsableModels     []string
}

type SupportedSEV struct {
	SEV         libvirtxml.DomainCapsFeatureSEV
	SupportedES string
}

func HostCapabilities(capabilitiesSource io.Reader) (*libvirtxml.Caps, error) {
	hostCapabilities, err := io.ReadAll(capabilitiesSource)
	if err != nil {
		return nil, err
	}
	var capabilities libvirtxml.Caps
	if err := capabilities.Unmarshal(string(hostCapabilities)); err != nil {
		return nil, err
	}
	return &capabilities, nil
}

func DomCapabilities(capabilitiesSource io.Reader) (*libvirtxml.DomainCaps, error) {
	hostDomCapabilities, err := io.ReadAll(capabilitiesSource)
	if err != nil {
		return nil, err
	}
	var capabilities libvirtxml.DomainCaps
	if err := capabilities.Unmarshal(string(hostDomCapabilities)); err != nil {
		return nil, err
	}
	return &capabilities, nil
}

func SupportedHostCPUs(cpuModes []libvirtxml.DomainCapsCPUMode) (*SupportedCPU, error) {
	var supportedCPU SupportedCPU
	for _, mode := range cpuModes {
		if virtconfig.IsAMD64(runtime.GOARCH) {
			log.Log.Warning("host-model cpu mode is not supported for ARM architecture")
			continue
		}

		// On s390x the xml does not include a CPU Vendor, however there is only one company selling them anyway.
		if virtconfig.IsS390X(runtime.GOARCH) && mode.Vendor == "" {
			supportedCPU.Vendor = "IBM"
		}

		if len(mode.Models) < 1 {
			return nil, fmt.Errorf("host model mode is expected to contain a model")
		}
		if len(mode.Models) > 1 {
			log.Log.Warning("host model mode is expected to contain only one model")
		}

		supportedCPU.Model = mode.Models[0]
		for _, feature := range mode.Features {
			if feature.Policy == "require" {
				supportedCPU.RequiredFeatures = append(supportedCPU.RequiredFeatures, feature)
			}
		}

		for _, model := range mode.Models {
			if model.Usable == "no" || model.Usable == "" {
				continue
			}
			supportedCPU.UsableModels = append(supportedCPU.UsableModels, model.Name)
		}
	}
	return &supportedCPU, nil
}

func SupportedHostSEV(sev libvirtxml.DomainCapsFeatureSEV) *SupportedSEV {
	var supportedSEV SupportedSEV
	supportedSEV.SEV = sev
	if sev.Supported == "yes" && sev.MaxESGuests > 0 {
		supportedSEV.SupportedES = "yes"
	} else {
		supportedSEV.SupportedES = "no"
	}
	return &supportedSEV
}
