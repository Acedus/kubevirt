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
	"context"
	"encoding/json"
	"fmt"
	"os"

	backupv1 "kubevirt.io/api/backup/v1alpha1"
	"kubevirt.io/client-go/log"

	osdisk "kubevirt.io/kubevirt/pkg/os/disk"
	"kubevirt.io/kubevirt/pkg/safepath"
	"kubevirt.io/kubevirt/pkg/storage/cbt"
	"kubevirt.io/kubevirt/pkg/storage/export/export"
)

const terminationLogPath = "/dev/termination-log"

// Keeps the report under the kubelet's 4096-byte termination-message cap, past which it would be
// truncated into invalid JSON.
const maxPrepareReportBytes = 3584

type offlineVolume struct {
	name       string
	overlay    *os.File
	data       *os.File
	dataDriver string
}

func RunOfflinePrepare(paths *export.ServerPaths, statePath, baseCheckpoint, checkpoint string) error {
	plan, err := prepareOfflineBackup(context.Background(), paths, statePath, baseCheckpoint, checkpoint)
	if err != nil {
		writeTerminationMessage([]byte(err.Error()))
		return err
	}
	payload, err := json.Marshal(plan)
	if err != nil {
		return err
	}
	if err := os.WriteFile(export.OfflineBackupPlanPath, payload, 0o644); err != nil {
		return fmt.Errorf("failed to write the offline backup plan: %w", err)
	}

	var fallbacks []backupv1.BackupVolumeInfo
	for _, volume := range plan {
		if volume.Type == backupv1.Full && baseCheckpoint != "" {
			fallbacks = append(fallbacks, volume)
		}
	}
	report, err := json.Marshal(fallbacks)
	if err != nil {
		return err
	}
	if len(report) > maxPrepareReportBytes {
		err := fmt.Errorf("offline backup volume type report is %d bytes, over the %d-byte termination-message budget", len(report), maxPrepareReportBytes)
		writeTerminationMessage([]byte(err.Error()))
		return err
	}
	writeTerminationMessage(report)
	return nil
}

func prepareOfflineBackup(ctx context.Context, paths *export.ServerPaths, statePath, baseCheckpoint, checkpoint string) ([]backupv1.BackupVolumeInfo, error) {
	if paths == nil || len(paths.Backups) == 0 {
		return nil, fmt.Errorf("offline backup requested but no backup volumes configured")
	}

	var plan []backupv1.BackupVolumeInfo
	for _, bi := range paths.Backups {
		vol, err := openOfflineVolume(bi.Path, statePath, os.O_RDWR)
		if err != nil {
			return nil, err
		}
		backupType, err := prepareOfflineVolume(ctx, vol, baseCheckpoint, checkpoint)
		vol.close()
		if err != nil {
			return nil, err
		}
		plan = append(plan, backupv1.BackupVolumeInfo{VolumeName: bi.Path, Type: backupType})
	}
	return plan, nil
}

func prepareOfflineVolume(ctx context.Context, vol *offlineVolume, baseCheckpoint, checkpoint string) (backupv1.BackupType, error) {
	info, err := osdisk.GetDiskInfoOfFile(ctx, vol.overlay, "qcow2")
	if err != nil {
		return "", fmt.Errorf("failed to inspect the CBT overlay of volume %s: %w", vol.name, err)
	}

	backupType := offlineBackupType(vol.name, info, baseCheckpoint)
	if checkpoint == "" {
		return backupType, nil
	}
	existing := info.Bitmap(checkpoint)
	if existing != nil && existing.Valid() {
		return backupType, nil
	}
	if existing != nil {
		if _, err := osdisk.QEMUImg(ctx, vol.files(), "bitmap", "--remove", "--image-opts", vol.imageOpts(), checkpoint); err != nil {
			return "", fmt.Errorf("failed to remove invalid checkpoint bitmap %s from volume %s: %w", checkpoint, vol.name, err)
		}
	}
	if _, err := osdisk.QEMUImg(ctx, vol.files(), "bitmap", "--add", "--image-opts", vol.imageOpts(), checkpoint); err != nil {
		return "", fmt.Errorf("failed to add checkpoint bitmap %s to volume %s: %w", checkpoint, vol.name, err)
	}
	return backupType, nil
}

func offlineBackupType(volumeName string, info *osdisk.DiskInfo, baseCheckpoint string) backupv1.BackupType {
	if baseCheckpoint == "" {
		return backupv1.Full
	}
	if base := info.Bitmap(baseCheckpoint); base != nil && base.Valid() {
		return backupv1.Incremental
	}
	log.Log.Infof("Volume %s has no usable bitmap for checkpoint %s; falling back to a full backup of this disk", volumeName, baseCheckpoint)
	return backupv1.Full
}

func openOfflineVolume(name, statePath string, flag int) (*offlineVolume, error) {
	overlay, err := openNoFollow(statePath, flag, cbt.GetQCOW2OverlayPVCPath(name))
	if err != nil {
		return nil, fmt.Errorf("failed to open the CBT overlay of volume %s: %w", name, err)
	}
	data, dataDriver, err := openVolumeData(export.OfflineBackupVolumePath(name), flag)
	if err != nil {
		_ = overlay.Close()
		return nil, fmt.Errorf("failed to open the data of volume %s: %w", name, err)
	}
	return &offlineVolume{
		name:       name,
		overlay:    overlay,
		data:       data,
		dataDriver: dataDriver,
	}, nil
}

// A block volume is the device itself, a filesystem volume holds its disk.img.
func openVolumeData(mountPath string, flag int) (*os.File, string, error) {
	mount, err := openNoFollow(mountPath, os.O_RDONLY)
	if err != nil {
		return nil, "", err
	}
	info, err := mount.Stat()
	_ = mount.Close()
	if err != nil {
		return nil, "", err
	}
	if !info.IsDir() {
		data, err := openNoFollow(mountPath, flag)
		return data, "host_device", err
	}
	data, err := openNoFollow(mountPath, flag, "disk.img")
	return data, "file", err
}

func openNoFollow(root string, flag int, elems ...string) (*os.File, error) {
	resolved, err := safepath.JoinAndResolveWithRelativeRoot(root, elems...)
	if err != nil {
		return nil, err
	}
	fd, err := safepath.OpenAtNoFollow(resolved)
	if err != nil {
		return nil, err
	}
	defer fd.Close()
	return os.OpenFile(fd.SafePath(), flag, 0)
}

// The qemu tools reach every file through the descriptors the exporter opened, so a symlink in a
// PVC can't redirect them.
func (v *offlineVolume) files() []*os.File {
	return []*os.File{v.overlay, v.data}
}

// The data-file path baked into the overlay points at the virt-launcher mount, which doesn't exist
// here, so qemu opens the data file through the image options instead.
func (v *offlineVolume) imageOpts() string {
	return fmt.Sprintf("driver=qcow2,file.driver=file,file.filename=%s,data-file.driver=%s,data-file.filename=%s",
		osdisk.ExtraFilePath(0), v.dataDriver, osdisk.ExtraFilePath(1))
}

func (v *offlineVolume) close() {
	_ = v.overlay.Close()
	_ = v.data.Close()
}

func writeTerminationMessage(message []byte) {
	if err := os.WriteFile(terminationLogPath, message, 0o644); err != nil {
		log.Log.Reason(err).Warning("failed to write termination log")
	}
}
