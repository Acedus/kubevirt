package storage

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"os/exec"
	"sync"
	"time"

	"kubevirt.io/client-go/log"
)

const (
	dialTimeout = 10 * time.Second
	keepalive   = 15 * time.Second
)

// --- PROTOCOL STRUCTS ---

type TunnelCommand struct {
	Op     string `json:"op"`     // "info", "map", "read"
	Disk   string `json:"disk"`   // Export name
	Offset int64  `json:"offset"` // For read
	Length int    `json:"length"` // For read
}

const (
	StatusSuccess = 0
	StatusError   = 1
)

// --- CONTROLLER ---

type BackupTunnelController struct {
	tunLock       sync.Mutex
	activeTunnel  *BackupTunnelManager
	currentTarget string
	currentCA     string
}

func NewBackupTunnelController() *BackupTunnelController {
	return &BackupTunnelController{}
}

func (btc *BackupTunnelController) StartOrUpdate(targetAddr, cacert string) {
	btc.tunLock.Lock()
	defer btc.tunLock.Unlock()

	if btc.activeTunnel != nil {
		if btc.currentTarget == targetAddr && btc.currentCA == cacert {
			return
		}
		log.Log.Infof("Stopping stale backup tunnel to %s", btc.currentTarget)
		btc.activeTunnel.Stop()
	}

	log.Log.Infof("Starting new backup tunnel to %s", targetAddr)
	btm := StartTunnel(targetAddr, cacert)

	btc.activeTunnel = btm
	btc.currentTarget = targetAddr
	btc.currentCA = cacert
}

func (btc *BackupTunnelController) Stop() {
	btc.tunLock.Lock()
	defer btc.tunLock.Unlock()

	if btc.activeTunnel != nil {
		log.Log.Info("Stopping backup tunnel.")
		btc.activeTunnel.Stop()
		btc.activeTunnel = nil
	}
}

// --- MANAGER ---

type BackupTunnelManager struct {
	ctx        context.Context
	cancel     context.CancelFunc
	targetAddr string
	cacert     string
}

func StartTunnel(targetAddr, cacert string) *BackupTunnelManager {
	ctx, cancel := context.WithCancel(context.Background())
	btm := &BackupTunnelManager{
		ctx:        ctx,
		cancel:     cancel,
		targetAddr: targetAddr,
		cacert:     cacert,
	}
	go btm.run()
	return btm
}

func (btm *BackupTunnelManager) Stop() {
	btm.cancel()
}

func (btm *BackupTunnelManager) run() {
	logger := log.Log
	backoff := 1 * time.Second

	for {
		select {
		case <-btm.ctx.Done():
			return
		default:
		}

		logger.Infof("Connecting to backup server: %s", btm.targetAddr)
		err := btm.establishSession()

		if err != nil {
			if btm.ctx.Err() != nil {
				return
			}
			logger.Reason(err).Errorf("Backup tunnel error. Retrying in %v", backoff)
			time.Sleep(backoff)
		}
	}
}

func (btm *BackupTunnelManager) establishSession() error {
	caCertPool := x509.NewCertPool()
	if !caCertPool.AppendCertsFromPEM([]byte(btm.cacert)) {
		return fmt.Errorf("failed to parse CA certificate")
	}

	tlsConfig := &tls.Config{
		RootCAs:    caCertPool,
		MinVersion: tls.VersionTLS12,
	}

	dialer := net.Dialer{
		Timeout:   dialTimeout,
		KeepAlive: keepalive,
	}

	conn, err := tls.DialWithDialer(&dialer, "tcp", btm.targetAddr, tlsConfig)
	if err != nil {
		return fmt.Errorf("tls dial failed: %w", err)
	}
	defer conn.Close()

	if err := performHandshake(conn); err != nil {
		return fmt.Errorf("handshake failed: %w", err)
	}

	return btm.processCommands(conn)
}

func performHandshake(conn net.Conn) error {
	conn.SetReadDeadline(time.Now().Add(5 * time.Second))
	buf := make([]byte, 3)
	if _, err := io.ReadFull(conn, buf); err != nil {
		return err
	}
	if string(buf) != "OK\n" {
		return fmt.Errorf("invalid handshake: %s", string(buf))
	}
	conn.SetDeadline(time.Time{})
	return nil
}

// --- WORKER LOGIC ---

func (btm *BackupTunnelManager) processCommands(conn net.Conn) error {
	decoder := json.NewDecoder(conn)
	headerBuf := make([]byte, 9) // [Status 1][Len 8]

	// Cache local NBD connections (DiskName -> *NBDClient)
	nbdClients := make(map[string]*NBDClient)
	defer func() {
		for _, c := range nbdClients {
			c.Close()
		}
	}()

	for {
		var cmd TunnelCommand
		if err := decoder.Decode(&cmd); err != nil {
			if err == io.EOF {
				return fmt.Errorf("server closed connection")
			}
			log.Log.Reason(err).Error("Failed to decode command")
			sendResponse(conn, headerBuf, StatusError, []byte("Invalid JSON"))
			return err
		}

		switch cmd.Op {
		case "map", "info":
			// Fallback to qemu-img for metadata operations (JSON output is complex to replicate)
			out, err := btm.executeQemuImgMeta(cmd)
			if err != nil {
				sendResponse(conn, headerBuf, StatusError, []byte(err.Error()))
			} else {
				sendResponse(conn, headerBuf, StatusSuccess, out)
			}

		case "read":
			// Use Native Go NBD Client for data path
			client, ok := nbdClients[cmd.Disk]
			if !ok {
				var err error
				client, err = NewNBDClient(pullBackupSocket, cmd.Disk)
				if err != nil {
					log.Log.Reason(err).Errorf("Failed to connect to NBD socket for disk %s", cmd.Disk)
					sendResponse(conn, headerBuf, StatusError, []byte(err.Error()))
					continue
				}
				nbdClients[cmd.Disk] = client
			}

			// Perform Read
			// 1. Seek
			if _, err := client.Seek(cmd.Offset, io.SeekStart); err != nil {
				sendResponse(conn, headerBuf, StatusError, []byte(fmt.Sprintf("seek failed: %v", err)))
				continue
			}

			// 2. Read into buffer
			// Note: We allocate buffer here. For high perf, consider a sync.Pool.
			buf := make([]byte, cmd.Length)
			n, err := io.ReadFull(client, buf)
			if err != nil && err != io.EOF {
				sendResponse(conn, headerBuf, StatusError, []byte(fmt.Sprintf("read failed: %v", err)))
				continue
			}

			// 3. Send Success
			sendResponse(conn, headerBuf, StatusSuccess, buf[:n])

		default:
			sendResponse(conn, headerBuf, StatusError, []byte("unknown op"))
		}
	}
}

func (btm *BackupTunnelManager) executeQemuImgMeta(cmd TunnelCommand) ([]byte, error) {
	nbdURI := fmt.Sprintf("nbd+unix:///%s?socket=%s", cmd.Disk, pullBackupSocket)
	args := []string{cmd.Op, "--output=json", nbdURI}
	c := exec.CommandContext(btm.ctx, "qemu-img", args...)
	return c.CombinedOutput()
}

func sendResponse(conn net.Conn, headerBuf []byte, status byte, data []byte) {
	headerBuf[0] = status
	binary.BigEndian.PutUint64(headerBuf[1:], uint64(len(data)))

	// Atomic-ish write: Header then Data
	conn.Write(headerBuf)
	conn.Write(data)
}

