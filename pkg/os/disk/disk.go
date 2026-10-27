package disk

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"slices"
)

type DiskInfo struct {
	Format         string            `json:"format"`
	BackingFile    string            `json:"backing-filename"`
	ActualSize     int64             `json:"actual-size"`
	VirtualSize    int64             `json:"virtual-size"`
	FormatSpecific ImageInfoSpecific `json:"format-specific"`
}

type ImageInfoSpecific struct {
	Data ImageInfoSpecificQCow2 `json:"data"`
}

type ImageInfoSpecificQCow2 struct {
	Bitmaps []Qcow2BitmapInfo `json:"bitmaps"`
}

type Qcow2BitmapInfo struct {
	Name  string   `json:"name"`
	Flags []string `json:"flags"`
}

// Valid reports whether the bitmap recorded every write: enabled, and stored cleanly.
func (b *Qcow2BitmapInfo) Valid() bool {
	return slices.Contains(b.Flags, "auto") && !slices.Contains(b.Flags, "in-use")
}

func (d *DiskInfo) Bitmap(name string) *Qcow2BitmapInfo {
	for i := range d.FormatSpecific.Data.Bitmaps {
		if d.FormatSpecific.Data.Bitmaps[i].Name == name {
			return &d.FormatSpecific.Data.Bitmaps[i]
		}
	}
	return nil
}

const (
	QEMUIMGPath = "/usr/bin/qemu-img"
)

func GetDiskInfo(imagePath string) (*DiskInfo, error) {
	// #nosec No risk for attacker injection. Only get information about an image
	out, err := QEMUImg(context.Background(), nil, "info", imagePath, "--output", "json")
	if err != nil {
		return nil, err
	}
	return parseDiskInfo(out)
}

// GetDiskInfoOfFile inspects an image through a file the caller already opened.
func GetDiskInfoOfFile(ctx context.Context, image *os.File, format string) (*DiskInfo, error) {
	out, err := QEMUImg(ctx, []*os.File{image}, "info", "-f", format, "--output", "json", ExtraFilePath(0))
	if err != nil {
		return nil, err
	}
	return parseDiskInfo(out)
}

func parseDiskInfo(out []byte) (*DiskInfo, error) {
	info := &DiskInfo{}
	if err := json.Unmarshal(out, info); err != nil {
		return nil, fmt.Errorf("failed to parse disk info: %v", err)
	}
	return info, nil
}

// ExtraFilePath is how a child process addresses the extra file at index.
func ExtraFilePath(index int) string {
	const stdioFdCount = 3
	return fmt.Sprintf("/proc/self/fd/%d", stdioFdCount+index)
}

func QEMUImg(ctx context.Context, extraFiles []*os.File, subcmd string, args ...string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, QEMUIMGPath, append([]string{subcmd}, args...)...)
	cmd.ExtraFiles = extraFiles
	out, err := cmd.Output()
	if err != nil {
		if exitErr, ok := errors.AsType[*exec.ExitError](err); ok {
			return nil, fmt.Errorf("qemu-img %s failed: %w: %s", subcmd, err, exitErr.Stderr)
		}
		return nil, fmt.Errorf("qemu-img %s failed: %w", subcmd, err)
	}
	return out, nil
}
