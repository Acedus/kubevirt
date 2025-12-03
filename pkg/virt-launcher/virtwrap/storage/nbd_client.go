package storage

import (
	"encoding/binary"
	"fmt"
	"io"
	"net"
	"sync"
)

const (
	NBD_FLAG_C_FIXED_NEWSTYLE = 1
	NBD_OPT_EXPORT_NAME       = 1
	NBD_REP_ACK               = 1
	NBD_CMD_READ              = 0

	NBD_REQUEST_MAGIC = 0x25609513
	NBD_REPLY_MAGIC   = 0x67446698
)

type NBDClient struct {
	conn   net.Conn
	offset int64
	size   int64
	mu     sync.Mutex
}

func NewNBDClient(socketPath, exportName string) (*NBDClient, error) {
	conn, err := net.Dial("unix", socketPath)
	if err != nil {
		return nil, err
	}

	client := &NBDClient{conn: conn}
	if err := client.handshake(exportName); err != nil {
		conn.Close()
		return nil, err
	}
	return client, nil
}

func (c *NBDClient) Close() error {
	return c.conn.Close()
}

func (c *NBDClient) Size() int64 {
	return c.size
}

// Seek implements io.Seeker
func (c *NBDClient) Seek(offset int64, whence int) (int64, error) {
	c.mu.Lock()
	defer c.mu.Unlock()

	var abs int64
	switch whence {
	case io.SeekStart:
		abs = offset
	case io.SeekCurrent:
		abs = c.offset + offset
	case io.SeekEnd:
		abs = c.size + offset
	default:
		return 0, fmt.Errorf("invalid whence")
	}

	if abs < 0 {
		return 0, fmt.Errorf("negative position")
	}
	c.offset = abs
	return abs, nil
}

// Read implements io.Reader
func (c *NBDClient) Read(p []byte) (int, error) {
	c.mu.Lock()
	defer c.mu.Unlock()

	if c.offset >= c.size {
		return 0, io.EOF
	}

	readLen := len(p)
	if c.offset+int64(readLen) > c.size {
		readLen = int(c.size - c.offset)
	}

	// 1. Send Request
	// Magic(4), Flags(2), Type(2), Handle(8), Offset(8), Length(4)
	req := make([]byte, 28)
	binary.BigEndian.PutUint32(req[0:4], NBD_REQUEST_MAGIC)
	binary.BigEndian.PutUint16(req[4:6], 0) // Flags
	binary.BigEndian.PutUint16(req[6:8], NBD_CMD_READ)
	binary.BigEndian.PutUint64(req[8:16], 0) // Handle
	binary.BigEndian.PutUint64(req[16:24], uint64(c.offset))
	binary.BigEndian.PutUint32(req[24:28], uint32(readLen))

	if _, err := c.conn.Write(req); err != nil {
		return 0, err
	}

	// 2. Receive Reply Header
	// Magic(4), Error(4), Handle(8)
	reply := make([]byte, 16)
	if _, err := io.ReadFull(c.conn, reply); err != nil {
		return 0, err
	}

	if magic := binary.BigEndian.Uint32(reply[0:4]); magic != NBD_REPLY_MAGIC {
		return 0, fmt.Errorf("invalid NBD reply magic")
	}
	if errCode := binary.BigEndian.Uint32(reply[4:8]); errCode != 0 {
		return 0, fmt.Errorf("NBD error: %d", errCode)
	}

	// 3. Receive Data
	n, err := io.ReadFull(c.conn, p[:readLen])
	if err != nil {
		return n, err
	}

	c.offset += int64(n)
	return n, nil
}

// handshake performs the QEMU NBD "Newstyle" negotiation
func (c *NBDClient) handshake(exportName string) error {
	// 1. Receive Init: "NBDMAGIC" (8) + "IHAVEOPT" (8) + Flags (2)
	header := make([]byte, 18)
	if _, err := io.ReadFull(c.conn, header); err != nil {
		return err
	}

	if string(header[0:8]) != "NBDMAGIC" || string(header[8:16]) != "IHAVEOPT" {
		return fmt.Errorf("invalid NBD server handshake")
	}

	// 2. Send Client Flags: C_FIXED_NEWSTYLE
	// QEMU expects 32 bits
	buf := make([]byte, 4)
	binary.BigEndian.PutUint32(buf, NBD_FLAG_C_FIXED_NEWSTYLE)
	if _, err := c.conn.Write(buf); err != nil {
		return err
	}

	// 3. Request Export: NBD_OPT_EXPORT_NAME
	// Magic(8), Opt(4), Len(4), Data(exportName)
	payloadLen := len(exportName)
	req := make([]byte, 16+payloadLen)
	binary.BigEndian.PutUint64(req[0:8], 0x49484156454F5054) // "IHAVEOPT"
	binary.BigEndian.PutUint32(req[8:12], NBD_OPT_EXPORT_NAME)
	binary.BigEndian.PutUint32(req[12:16], uint32(payloadLen))
	copy(req[16:], exportName)

	if _, err := c.conn.Write(req); err != nil {
		return err
	}

	// 4. Receive Export Info
	// Size(8), Flags(2), Zeroes(124)
	info := make([]byte, 134)
	if _, err := io.ReadFull(c.conn, info); err != nil {
		return err
	}

	c.size = int64(binary.BigEndian.Uint64(info[0:8]))
	return nil
}
