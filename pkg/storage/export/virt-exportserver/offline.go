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
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"time"

	"k8s.io/apimachinery/pkg/util/wait"

	backupv1 "kubevirt.io/api/backup/v1alpha1"
	"kubevirt.io/client-go/log"

	osdisk "kubevirt.io/kubevirt/pkg/os/disk"
	"kubevirt.io/kubevirt/pkg/safepath"
	"kubevirt.io/kubevirt/pkg/storage/cbt"
	"kubevirt.io/kubevirt/pkg/storage/export/export"
	"kubevirt.io/kubevirt/pkg/storage/nbdclient"
)

const (
	terminationLogPath    = "/dev/termination-log"
	qemuStorageDaemonPath = "/usr/bin/qemu-storage-daemon"

	servingPollInterval = time.Second
	servingTimeout      = time.Minute
)

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

// RunOfflineServe replaces the process with a qemu-storage-daemon serving every planned volume on
// socket, exporting the base checkpoint's bitmap for each incremental one.
func RunOfflineServe(statePath, socket, baseCheckpoint string) error {
	plan, err := readOfflinePlan(export.OfflineBackupPlanPath)
	if err != nil {
		return err
	}

	args := []string{qemuStorageDaemonPath, "--nbd-server", "addr.type=unix,addr.path=" + socket}
	for i, volume := range plan {
		dataPath, dataDriver, err := volumeDataPath(export.OfflineBackupVolumePath(volume.VolumeName))
		if err != nil {
			return fmt.Errorf("failed to find the data of volume %s: %w", volume.VolumeName, err)
		}
		node := fmt.Sprintf("vol%d", i)
		args = append(args, "--blockdev", fmt.Sprintf(
			"node-name=%s,read-only=on,driver=qcow2,file.driver=file,file.filename=%s,data-file.driver=%s,data-file.filename=%s",
			node, filepath.Join(statePath, cbt.GetQCOW2OverlayPVCPath(volume.VolumeName)), dataDriver, dataPath))
		nbdExport := fmt.Sprintf("type=nbd,id=%s,node-name=%s,name=%s", node, node, volume.VolumeName)
		if volume.Type == backupv1.Incremental {
			nbdExport += ",bitmaps.0=" + baseCheckpoint
		}
		args = append(args, "--export", nbdExport)
	}

	if err := os.Remove(socket); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("failed to remove stale socket %s: %w", socket, err)
	}
	return syscall.Exec(qemuStorageDaemonPath, args, os.Environ())
}

// ApplyOfflineBackupPlan sets each volume's backup type to the one prepare decided on.
func ApplyOfflineBackupPlan(paths *export.ServerPaths, planPath string) error {
	plan, err := readOfflinePlan(planPath)
	if err != nil {
		return err
	}
	for _, volume := range plan {
		if i := slices.IndexFunc(paths.Backups, func(bi export.BackupInfo) bool { return bi.Path == volume.VolumeName }); i >= 0 {
			paths.Backups[i].Type = string(volume.Type)
		}
	}
	return nil
}

func readOfflinePlan(path string) ([]backupv1.BackupVolumeInfo, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("failed to read the offline backup plan: %w", err)
	}
	var plan []backupv1.BackupVolumeInfo
	if err := json.Unmarshal(raw, &plan); err != nil {
		return nil, fmt.Errorf("failed to parse the offline backup plan: %w", err)
	}
	return plan, nil
}

func volumeDataPath(mountPath string) (string, string, error) {
	info, err := os.Stat(mountPath)
	if err != nil {
		return "", "", err
	}
	if !info.IsDir() {
		return mountPath, "host_device", nil
	}
	return filepath.Join(mountPath, "disk.img"), "file", nil
}

// RunOfflinePush copies every planned volume from the NBD socket into targetDir, in the layout of
// an online push backup.
func RunOfflinePush(planPath, socket, bitmap, targetDir, backupName string) error {
	err := runOfflinePush(context.Background(), planPath, socket, bitmap, targetDir, backupName)
	if err != nil {
		writeTerminationMessage([]byte(err.Error()))
	}
	return err
}

func runOfflinePush(ctx context.Context, planPath, socket, bitmap, targetDir, backupName string) error {
	plan, err := readOfflinePlan(planPath)
	if err != nil {
		return err
	}

	client := nbdclient.NewNBDClient(socket)
	if err := wait.PollUntilContextTimeout(ctx, servingPollInterval, servingTimeout, true, func(context.Context) (bool, error) {
		return client.Serving(servingPollInterval) == nil, nil
	}); err != nil {
		return fmt.Errorf("no NBD server on %s: %w", socket, err)
	}

	dir, err := mkdirAllNoFollow(export.OfflineBackupTargetPath, targetDir)
	if err != nil {
		return fmt.Errorf("failed to create backup target dir %s: %w", targetDir, err)
	}
	for _, volume := range plan {
		name := fmt.Sprintf("%s-%s.qcow2", backupName, volume.VolumeName)
		volumeBitmap := ""
		if volume.Type == backupv1.Incremental {
			volumeBitmap = bitmap
		}
		if err := pushVolume(ctx, client, socket, dir, volume.VolumeName, name, volumeBitmap); err != nil {
			return err
		}
		log.Log.Infof("Offline push backup wrote %s", filepath.Join(targetDir, name))
	}
	return nil
}

func pushVolume(ctx context.Context, client *nbdclient.NBDClient, socket string, dir *safepath.Path, volume, name, bitmap string) error {
	target, err := createNoFollow(dir, name)
	if err != nil {
		return fmt.Errorf("failed to create backup target %s: %w", name, err)
	}
	defer target.Close()
	targetFiles := []*os.File{target}

	if bitmap == "" {
		srcOpts := fmt.Sprintf("driver=nbd,server.type=unix,server.path=%s,export=%s", socket, volume)
		if _, err := osdisk.QEMUImg(ctx, targetFiles, "convert", "--image-opts", srcOpts, "-O", "qcow2", osdisk.ExtraFilePath(0)); err != nil {
			return fmt.Errorf("failed to copy volume %s: %w", volume, err)
		}
		return nil
	}

	var size uint64
	var dirty []nbdclient.Extent
	for extent, err := range client.Map(ctx, volume, bitmap, 0, 0) {
		if err != nil {
			return fmt.Errorf("failed to map volume %s: %w", volume, err)
		}
		size = extent.Offset + extent.Length
		if extent.Dirty() {
			dirty = append(dirty, extent)
		}
	}

	if _, err := osdisk.QEMUImg(ctx, targetFiles, "create", "-f", "qcow2", osdisk.ExtraFilePath(0), strconv.FormatUint(size, 10)); err != nil {
		return fmt.Errorf("failed to create incremental target %s: %w", name, err)
	}
	for _, e := range dirty {
		srcOpts := fmt.Sprintf("driver=raw,offset=%d,size=%d,file.driver=nbd,file.server.type=unix,file.server.path=%s,file.export=%s",
			e.Offset, e.Length, socket, volume)
		dstOpts := fmt.Sprintf("driver=raw,offset=%d,file.driver=qcow2,file.file.driver=file,file.file.filename=%s", e.Offset, osdisk.ExtraFilePath(0))
		if _, err := osdisk.QEMUImg(ctx, targetFiles, "convert", "-n", "--image-opts", srcOpts, "--target-image-opts", dstOpts); err != nil {
			return fmt.Errorf("failed to copy extent [%d,%d) of volume %s: %w", e.Offset, e.Offset+e.Length, volume, err)
		}
	}
	return nil
}

func mkdirAllNoFollow(root, dir string) (*safepath.Path, error) {
	rel, err := filepath.Rel(root, dir)
	if err != nil || rel == ".." || strings.HasPrefix(rel, "../") {
		return nil, fmt.Errorf("%s is outside of %s", dir, root)
	}
	current, err := safepath.JoinAndResolveWithRelativeRoot(root)
	if err != nil {
		return nil, err
	}
	for _, element := range strings.Split(rel, string(filepath.Separator)) {
		if element == "." {
			continue
		}
		if err := safepath.MkdirAtNoFollow(current, element, 0o755); err != nil && !errors.Is(err, syscall.EEXIST) {
			return nil, err
		}
		if current, err = safepath.JoinNoFollow(current, element); err != nil {
			return nil, err
		}
	}
	return current, nil
}

func createNoFollow(dir *safepath.Path, name string) (*os.File, error) {
	if err := safepath.TouchAtNoFollow(dir, name, 0o644); err != nil && !errors.Is(err, syscall.EEXIST) {
		return nil, err
	}
	path, err := safepath.JoinNoFollow(dir, name)
	if err != nil {
		return nil, err
	}
	fd, err := safepath.OpenAtNoFollow(path)
	if err != nil {
		return nil, err
	}
	defer fd.Close()
	return os.OpenFile(fd.SafePath(), os.O_RDWR, 0)
}

func writeTerminationMessage(message []byte) {
	if err := os.WriteFile(terminationLogPath, message, 0o644); err != nil {
		log.Log.Reason(err).Warning("failed to write termination log")
	}
}
