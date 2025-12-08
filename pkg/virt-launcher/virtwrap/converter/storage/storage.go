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

package storage

import (
	v1 "kubevirt.io/api/core/v1"

	"kubevirt.io/kubevirt/pkg/virt-launcher/virtwrap/api"
)

type DomainConfigurator struct {
	vcpus    uint
	useBlkMQ bool
}

type option func(*DomainConfigurator)

func NewDomainConfigurator(options ...option) DomainConfigurator {
	var configurator DomainConfigurator

	for _, f := range options {
		f(&configurator)
	}

	return configurator
}

func (d DomainConfigurator) Configure(vmi *v1.VirtualMachineInstance, domain *api.Domain) error {
	return nil
}

func WithUseBlkMQ(useBlkMQ bool) option {
	return func(d *DomainConfigurator) {
		d.useBlkMQ = useBlkMQ
	}
}

func WithVcpus(count uint) option {
	return func(d *DomainConfigurator) {
		d.vcpus = count
		if d.vcpus == 0 {
			d.vcpus = 1
		}
	}
}
