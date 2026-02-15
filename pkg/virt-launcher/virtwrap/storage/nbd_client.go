package storage

import (
	"context"
	"fmt"

	"libguestfs.org/libnbd"

	nbdv1 "kubevirt.io/kubevirt/pkg/nbd-com/nbd/v1"
)

type NBDClient struct {
	socketPath string
}

func NewNBDClient(socketPath string) *NBDClient {
	return &NBDClient{socketPath: socketPath}
}

func (s *NBDClient) GetExportInfo(ctx context.Context, req *nbdv1.ExportInfoRequest) (*nbdv1.ExportInfoResponse, error) {
	h, err := libnbd.Create()
	if err != nil {
		return nil, fmt.Errorf("failed to create nbd handle: %v", err)
	}
	defer h.Close()

	if err := h.SetExportName(req.ExportName); err != nil {
		return nil, err
	}

	if err := h.ConnectUnix(s.socketPath); err != nil {
		return nil, fmt.Errorf("failed to connect to %s: %v", s.socketPath, err)
	}

	size, err := h.GetSize()
	if err != nil {
		return nil, err
	}

	return &nbdv1.ExportInfoResponse{
		Size: int64(size),
	}, nil
}

func (s *NBDClient) GetDirtyBitmap(ctx context.Context, req *nbdv1.MapRequest) (*nbdv1.MapResponse, error) {
	h, err := libnbd.Create()
	if err != nil {
		return nil, err
	}
	defer h.Close()

	bitmapContext := libnbd.CONTEXT_QEMU_DIRTY_BITMAP + req.BitmapName
	if err := h.AddMetaContext(bitmapContext); err != nil {
		return nil, fmt.Errorf("bitmap context not supported: %v", err)
	}

	if err := h.SetExportName(req.ExportName); err != nil {
		return nil, err
	}
	if err := h.ConnectUnix(s.socketPath); err != nil {
		return nil, err
	}

	size, _ := h.GetSize()
	var regions []*nbdv1.DirtyRegion
	var currentOffset uint64 = 0

	for currentOffset < size {
		remaining := size - currentOffset

		err = h.BlockStatus(remaining, currentOffset, func(metacontext string, offset uint64, entries []uint32, error *int) int {
			if metacontext == bitmapContext {
				for i := 0; i < len(entries); i += 2 {
					length := uint64(entries[i])
					flags := entries[i+1]

					if (flags & libnbd.STATE_DIRTY) != 0 {
						regions = append(regions, &nbdv1.DirtyRegion{
							Offset: int64(offset),
							Length: int64(length),
						})
					}

					offset += length
					currentOffset = offset
				}
			}
			return 0
		}, nil)

		if err != nil {
			return nil, fmt.Errorf("block status failed at %d: %v", currentOffset, err)
		}
	}

	return &nbdv1.MapResponse{Regions: regions}, nil
}

func (s *NBDClient) Read(req *nbdv1.ReadRequest, stream nbdv1.NBD_ReadServer) error {
	h, err := libnbd.Create()
	if err != nil {
		return err
	}
	defer h.Close()

	if err := h.SetExportName(req.ExportName); err != nil {
		return err
	}

	if err := h.ConnectUnix(s.socketPath); err != nil {
		return err
	}

	const maxChunkSize = 2 * 1024 * 1024 // 2MB chunks for gRPC stability
	buf := make([]byte, maxChunkSize)
	remaining := req.Length
	offset := uint64(req.Offset)

	for remaining > 0 {
		toRead := uint32(maxChunkSize)
		if remaining < int64(toRead) {
			toRead = uint32(remaining)
		}

		err := h.Pread(buf[:toRead], offset, nil)
		if err != nil {
			return fmt.Errorf("nbd read failed at offset %d: %v", offset, err)
		}

		// Encapsulate and stream
		if err := stream.Send(&nbdv1.DataChunk{Data: buf}); err != nil {
			return err
		}

		offset += uint64(len(buf))
		remaining -= int64(len(buf))
	}

	return nil
}
