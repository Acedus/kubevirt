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

package main

import (
	"os"

	"kubevirt.io/client-go/log"

	exportServer "kubevirt.io/kubevirt/pkg/storage/export/virt-exportserver"
)

func main() {
	log.InitializeLogging("virt-backup-push")

	plan := os.Getenv("BACKUP_PLAN")
	socket := os.Getenv("BACKUP_NBD_SOCKET")
	targetDir := os.Getenv("BACKUP_TARGET_DIR")
	name := os.Getenv("BACKUP_NAME")
	if plan == "" || socket == "" || targetDir == "" || name == "" {
		log.Log.Error("no backup plan, NBD socket, target dir or backup name provided")
		os.Exit(1)
	}
	if err := exportServer.RunOfflinePush(plan, socket, os.Getenv("BACKUP_CHECKPOINT"), targetDir, name); err != nil {
		log.Log.Reason(err).Error("offline push backup failed")
		os.Exit(1)
	}
}
