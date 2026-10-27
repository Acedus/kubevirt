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

package virtexportserver

import (
	"os"
	"path/filepath"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

var _ = Describe("Offline backup", func() {
	Describe("openVolumeData", func() {
		It("opens a filesystem volume's disk image", func() {
			mount := GinkgoT().TempDir()
			Expect(os.WriteFile(filepath.Join(mount, "disk.img"), nil, 0o644)).To(Succeed())

			data, driver, err := openVolumeData(mount, os.O_RDONLY)
			Expect(err).ToNot(HaveOccurred())
			DeferCleanup(data.Close)
			Expect(driver).To(Equal("file"))
		})

		It("refuses a disk image that is a symlink", func() {
			mount := GinkgoT().TempDir()
			outside := filepath.Join(GinkgoT().TempDir(), "secret")
			Expect(os.WriteFile(outside, nil, 0o644)).To(Succeed())
			Expect(os.Symlink(outside, filepath.Join(mount, "disk.img"))).To(Succeed())

			_, _, err := openVolumeData(mount, os.O_RDONLY)
			Expect(err).To(HaveOccurred())
		})

		It("errors when the volume isn't mounted", func() {
			_, _, err := openVolumeData(filepath.Join(GinkgoT().TempDir(), "missing"), os.O_RDONLY)
			Expect(err).To(HaveOccurred())
		})
	})

	Describe("mkdirAllNoFollow", func() {
		It("creates the target dir under the root", func() {
			root := GinkgoT().TempDir()

			_, err := mkdirAllNoFollow(root, filepath.Join(root, "vm", "backup"))
			Expect(err).ToNot(HaveOccurred())
			Expect(filepath.Join(root, "vm", "backup")).To(BeADirectory())
		})

		It("refuses a target dir outside of the root", func() {
			root := GinkgoT().TempDir()

			_, err := mkdirAllNoFollow(root, filepath.Dir(root))
			Expect(err).To(HaveOccurred())
		})

		It("refuses to follow a symlink in the target dir", func() {
			root := GinkgoT().TempDir()
			Expect(os.Symlink(GinkgoT().TempDir(), filepath.Join(root, "vm"))).To(Succeed())

			_, err := mkdirAllNoFollow(root, filepath.Join(root, "vm", "backup"))
			Expect(err).To(HaveOccurred())
		})
	})
})
