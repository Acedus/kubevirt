package storage

import (
	"bufio"
	"context"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"os/exec"
	"strings"
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
	Op     string `json:"op"`     // "read" or "map"
	Disk   string `json:"disk"`   // Export name (e.g., "vda")
	Offset int64  `json:"offset"` // For read
	Length int    `json:"length"` // For read
}

const (
	StatusSuccess = 0
	StatusError   = 1
)

// --- CONTROLLER (Lifecycle) ---

type BackupTunnelController struct {
	tunLock       sync.Mutex
	activeTunnel  *BackupTunnelManager
	currentTarget string
	currentToken  string
}

func NewBackupTunnelController() *BackupTunnelController {
	return &BackupTunnelController{}
}

func (btc *BackupTunnelController) StartOrUpdate(targetAddr, token string) {
	btc.tunLock.Lock()
	defer btc.tunLock.Unlock()

	if btc.activeTunnel != nil {
		if btc.currentTarget == targetAddr && btc.currentToken == token {
			return // Idempotent
		}
		log.Log.Infof("Stopping stale backup tunnel to %s", btc.currentTarget)
		btc.activeTunnel.Stop()
	}

	log.Log.Infof("Starting new backup tunnel to %s", targetAddr)
	btm := StartTunnel(targetAddr, token)

	btc.activeTunnel = btm
	btc.currentTarget = targetAddr
	btc.currentToken = token
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

// --- MANAGER (Retry Loop) ---

type BackupTunnelManager struct {
	ctx        context.Context
	cancel     context.CancelFunc
	targetAddr string
	token      string
}

func StartTunnel(targetAddr, token string) *BackupTunnelManager {
	ctx, cancel := context.WithCancel(context.Background())
	btm := &BackupTunnelManager{
		ctx:        ctx,
		cancel:     cancel,
		targetAddr: targetAddr,
		token:      token,
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
	dialer := net.Dialer{
		Timeout:   dialTimeout,
		KeepAlive: keepalive,
	}

	conn, err := dialer.DialContext(btm.ctx, "tcp", btm.targetAddr)
	if err != nil {
		return fmt.Errorf("dial failed: %w", err)
	}
	defer conn.Close()

	if err := performHandshake(conn, btm.token); err != nil {
		return fmt.Errorf("handshake failed: %w", err)
	}

	// Enter the Command Processing Loop
	// This blocks until the connection dies or context is canceled
	return btm.processCommands(conn)
}

func performHandshake(conn net.Conn, token string) error {
	cleanToken := strings.TrimSpace(token)
	payload := fmt.Sprintf("%s\n", cleanToken)

	conn.SetWriteDeadline(time.Now().Add(5 * time.Second))
	if _, err := conn.Write([]byte(payload)); err != nil {
		return err
	}

	conn.SetReadDeadline(time.Now().Add(5 * time.Second))
	buf := make([]byte, 3)
	if _, err := io.ReadFull(conn, buf); err != nil {
		return err
	}
	if string(buf) != "OK\n" {
		return fmt.Errorf("invalid handshake: %s", string(buf))
	}

	conn.SetDeadline(time.Time{}) // Clear deadlines for long-lived connection
	return nil
}

// --- WORKER LOGIC (The meat of the changes) ---

func (btm *BackupTunnelManager) processCommands(conn net.Conn) error {
	scanner := bufio.NewScanner(conn)
	// Header buffer: [Status 1][Length 8]
	headerBuf := make([]byte, 9)

	for scanner.Scan() {
		// 1. Read Command JSON
		var cmd TunnelCommand
		if err := json.Unmarshal(scanner.Bytes(), &cmd); err != nil {
			log.Log.Reason(err).Error("Failed to unmarshal backup command")
			sendResponse(conn, headerBuf, StatusError, []byte(err.Error()))
			continue
		}

		// 2. Execute Command
		out, err := btm.executeQemuImg(cmd)

		// 3. Send Response
		if err != nil {
			sendResponse(conn, headerBuf, StatusError, []byte(err.Error()))
		} else {
			sendResponse(conn, headerBuf, StatusSuccess, out)
		}
	}
	return scanner.Err()
}

func (btm *BackupTunnelManager) executeQemuImg(cmd TunnelCommand) ([]byte, error) {
	// We use the NBD socket exposed by QEMU in the Pod
	// URI format: nbd+unix://<export>?socket=<path>
	// BUT qemu-img expects --image-opts for granular control

	imgOpts := fmt.Sprintf("driver=nbd,server.type=unix,server.path=%s,export=%s", pullBackupSocket, cmd.Disk)

	var args []string

	switch cmd.Op {
	case "read":
		// qemu-img dd -f raw --image-opts ... bs=1 skip=X count=Y
		args = []string{
			"dd", "-f", "raw",
			"--image-opts", imgOpts,
			"bs=1",
			fmt.Sprintf("skip=%d", cmd.Offset),
			fmt.Sprintf("count=%d", cmd.Length),
		}
	case "map":
		// qemu-img map --output=json --image-opts ...
		args = []string{
			"map", "--output=json",
			"--image-opts", imgOpts,
		}
	default:
		return nil, fmt.Errorf("unknown op: %s", cmd.Op)
	}

	// Run command
	// Note: This captures stdout into memory. For very large chunks (>10MB),
	// this might be memory intensive, but standard backup chunks are usually 1MB.
	c := exec.CommandContext(btm.ctx, "qemu-img", args...)
	return c.Output()
}

func sendResponse(conn net.Conn, headerBuf []byte, status byte, data []byte) {
	// Header: [Status 1][Length 8]
	headerBuf[0] = status
	binary.BigEndian.PutUint64(headerBuf[1:], uint64(len(data)))

	// We must write Header + Data atomically to the TCP stream
	// (Though strictly speaking, TCP is a stream, so sequential writes are fine
	// as long as we are single-threaded here)
	conn.Write(headerBuf)
	conn.Write(data)
}
