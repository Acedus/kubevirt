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

package disk

import (
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

var _ = Describe("DiskInfo", func() {
	info := func() *DiskInfo {
		info, err := parseDiskInfo([]byte(`{
			"format": "qcow2",
			"format-specific": {"type": "qcow2", "data": {"bitmaps": [
				{"name": "clean", "flags": ["auto"], "granularity": 65536},
				{"name": "dirty", "flags": ["in-use", "auto"], "granularity": 65536},
				{"name": "disabled", "flags": [], "granularity": 65536}
			]}}
		}`))
		Expect(err).ToNot(HaveOccurred())
		return info
	}

	It("finds a bitmap by name", func() {
		Expect(info().Bitmap("clean")).ToNot(BeNil())
		Expect(info().Bitmap("missing")).To(BeNil())
	})

	It("only trusts an enabled bitmap QEMU stored cleanly", func() {
		Expect(info().Bitmap("clean").Valid()).To(BeTrue())
		Expect(info().Bitmap("dirty").Valid()).To(BeFalse())
		Expect(info().Bitmap("disabled").Valid()).To(BeFalse())
	})
})
