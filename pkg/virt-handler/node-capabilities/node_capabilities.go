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
	"libvirt.org/go/libvirtxml"
)

const (
	CapabilitiesVolumePath   = "/var/lib/kubevirt-node-labeller/"
	HostCapabilitiesFilename = "capabilities.xml"
)

func HostCapabilities(hostCapabilities string) (*libvirtxml.Caps, error) {
	var capabilities libvirtxml.Caps
	if err := capabilities.Unmarshal(hostCapabilities); err != nil {
		return nil, err
	}
	return &capabilities, nil
}
