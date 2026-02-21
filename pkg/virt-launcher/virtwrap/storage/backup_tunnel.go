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

package storage

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/fsnotify/fsnotify"
	"google.golang.org/grpc"
	"google.golang.org/grpc/keepalive"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/utils/clock"
	"kubevirt.io/client-go/log"

	nbdv1 "kubevirt.io/kubevirt/pkg/storage/cbt/nbd/v1"
)

const (
	defaultKeepaliveMinTime        = 5 * time.Second
	defaultDialTimeout             = 10 * time.Second
	defaultHandshakeTimeout        = 10 * time.Second
	defaultGracefulShutdownTimeout = 10 * time.Second
	defaultExportServerPort        = 9090

	defaultTunnelInit          = 1 * time.Second
	defaultTunnelCap           = 5 * time.Minute
	defaultTunnelReset         = 30 * time.Second
	defaultTunnelBackoffFactor = 2.0
	defaultTunnelBackoffJitter = 0.1
)

type backupTunnelManager struct {
	targetAddr string
	nbdSocket  string
	token      string
	tlsConfig  *tls.Config

	mu     sync.Mutex
	server *grpc.Server
}

func newBackupTunnelManager(targetAddr, serverName, nbdSocket, token string, caCert []byte) (*backupTunnelManager, error) {
	if err := canCreateTunnel(targetAddr, serverName, nbdSocket, token); err != nil {
		return nil, err
	}
	certPool := x509.NewCertPool()
	if ok := certPool.AppendCertsFromPEM(caCert); !ok {
		return nil, fmt.Errorf("failed to parse CA certificate: no valid PEM blocks found")
	}
	return &backupTunnelManager{
		targetAddr: targetAddr,
		nbdSocket:  nbdSocket,
		token:      token,
		tlsConfig: &tls.Config{
			RootCAs:            certPool,
			ServerName:         serverName,
			InsecureSkipVerify: false,
		},
	}, nil
}

func (m *backupTunnelManager) Start() error {
	// TODO: add TTL handling by deriving from JWT token
	ctx := context.Background()
	nbdSocketCh, err := m.watchSocket(ctx)
	if err != nil {
		return fmt.Errorf("failed to initialize socket watcher: %w", err)
	}

	go func() {
		if err := m.run(ctx, nbdSocketCh); err != nil {
			log.Log.Reason(err).Error("backup tunnel stopped with terminal error")
		}
	}()

	return nil
}

func (m *backupTunnelManager) Stop() {
	m.stopServer()
}

func (m *backupTunnelManager) run(ctx context.Context, nbdSocketCh <-chan struct{}) error {
	// the virt-exportserver may not be listening yet when we try to connect
	// or the connection may have terminated abruptly.
	// try establishing the connection with an exponential backoff and
	// reset the duration if enough time passes without a connection failure after establishing it
	delayFn := wait.Backoff{
		Duration: defaultTunnelInit,
		Cap:      defaultTunnelCap,
		Factor:   defaultTunnelBackoffFactor,
		Jitter:   defaultTunnelBackoffJitter,
	}.DelayWithReset(&clock.RealClock{}, defaultTunnelReset)

	err := delayFn.Until(ctx, true, true, func(ctx context.Context) (bool, error) {
		select {
		case <-nbdSocketCh:
			log.Log.Infof("NBD socket %s removed, stopping backup tunnel", m.nbdSocket)
			return false, fmt.Errorf("NBD socket removed")
		default:
			if _, err := os.Stat(m.nbdSocket); errors.Is(err, os.ErrNotExist) {
				log.Log.Infof("NBD socket %s not found, stopping backup tunnel", m.nbdSocket)
				return false, fmt.Errorf("NBD socket not found")
			}
			if err := m.establishAndServe(ctx, nbdSocketCh); err != nil {
				log.Log.Reason(err).Warning("backup tunnel connection lost, retrying")
				return false, nil
			}
		}
		return true, nil
	})

	if err != nil && !errors.Is(err, context.Canceled) {
		log.Log.Reason(err).Error("backup tunnel stopped with terminal error")
	}

	m.stopServer()
	return nil
}

func (m *backupTunnelManager) stopServer() {
	m.mu.Lock()
	s := m.server
	m.mu.Unlock()
	if s != nil {
		s.Stop()
	}
}

func (m *backupTunnelManager) establishAndServe(ctx context.Context, nbdSocketCh <-chan struct{}) error {
	addr := fmt.Sprintf("%s:%d", m.targetAddr, defaultExportServerPort)

	conn, err := m.dial(addr)
	if err != nil {
		return fmt.Errorf("dial %s: %w", addr, err)
	}
	defer conn.Close()
	if err := m.handshake(conn); err != nil {
		return fmt.Errorf("handshake: %w", err)
	}

	srv := grpc.NewServer(
		grpc.KeepaliveEnforcementPolicy(keepalive.EnforcementPolicy{
			MinTime:             defaultKeepaliveMinTime,
			PermitWithoutStream: true,
		}),
	)
	nbdv1.RegisterNBDServer(srv, NewNBDClient(m.nbdSocket))

	m.mu.Lock()
	m.server = srv
	m.mu.Unlock()

	log.Log.Infof("backup tunnel: connected to %s, serving NBD over gRPC", addr)

	serveDone := make(chan struct{})
	defer close(serveDone)

	go func() {
		select {
		case <-ctx.Done():
			log.Log.Info("context done, closing backup tunnel")
		case <-nbdSocketCh:
			log.Log.Info("NBD socket gone, closing backup tunnel")
		case <-serveDone:
			return
		}

		gracefulDone := make(chan struct{})
		go func() {
			defer close(gracefulDone)
			srv.GracefulStop()
		}()

		select {
		case <-gracefulDone:
		case <-time.After(defaultGracefulShutdownTimeout):
			log.Log.Warning("backup tunnel graceful shutdown timeout, forcing stop")
			srv.Stop()
		}
	}()

	closed := make(chan struct{})
	wrapped := &closeNotifyConn{Conn: conn, closed: closed}
	if err := srv.Serve(&oneConnListener{
		conn:   wrapped,
		closed: closed,
		addr:   conn.LocalAddr(),
	}); err != nil && !errors.Is(err, net.ErrClosed) && !errors.Is(err, io.EOF) {
		return err
	}

	m.mu.Lock()
	m.server = nil
	m.mu.Unlock()

	select {
	case <-ctx.Done():
		return nil
	case <-nbdSocketCh:
		return nil
	default:
		return fmt.Errorf("remote closed connection unexpectedly")
	}
}

func (m *backupTunnelManager) dial(addr string) (*tls.Conn, error) {
	return tls.DialWithDialer(&net.Dialer{Timeout: defaultDialTimeout}, "tcp", addr, m.tlsConfig)
}

func (m *backupTunnelManager) handshake(conn net.Conn) error {
	deadline := time.Now().Add(defaultHandshakeTimeout)

	if err := conn.SetWriteDeadline(deadline); err != nil {
		return fmt.Errorf("set write deadline: %w", err)
	}
	if _, err := fmt.Fprintf(conn, "%s\n", m.token); err != nil {
		return fmt.Errorf("send token: %w", err)
	}
	conn.SetWriteDeadline(time.Time{})

	if err := conn.SetReadDeadline(deadline); err != nil {
		return fmt.Errorf("set read deadline: %w", err)
	}

	respStr, err := readUnbufferedLine(conn)
	conn.SetReadDeadline(time.Time{})
	if err != nil {
		return fmt.Errorf("read response: %w", err)
	}

	if respStr != "OK" {
		return fmt.Errorf("server rejected token: %q", respStr)
	}
	return nil
}

func readUnbufferedLine(r io.Reader) (string, error) {
	var resp []byte
	var buf [1]byte
	for len(resp) < 1024 {
		_, err := r.Read(buf[:])
		if err != nil {
			return string(resp), err
		}
		if buf[0] == '\n' {
			return strings.TrimSuffix(string(resp), "\r"), nil
		}
		resp = append(resp, buf[0])
	}
	return "", fmt.Errorf("line length limited to 1024 bytes")
}

func (m *backupTunnelManager) watchSocket(ctx context.Context) (<-chan struct{}, error) {
	watcher, err := fsnotify.NewWatcher()
	if err != nil {
		return nil, fmt.Errorf("failed to create inotify watcher: %w", err)
	}
	socketDir := filepath.Dir(m.nbdSocket)
	if err := watcher.Add(socketDir); err != nil {
		watcher.Close()
		return nil, fmt.Errorf("failed to watch directory %s: %w", socketDir, err)
	}
	ch := make(chan struct{})

	go func() {
		defer func() {
			watcher.Close()
			close(ch)
			log.Log.Infof("backup tunnel socket watcher stopped for %s", m.nbdSocket)
		}()

		for {
			select {
			case <-ctx.Done():
				return
			case event, ok := <-watcher.Events:
				if !ok {
					return
				}
				if event.Name == m.nbdSocket && event.Op&(fsnotify.Remove|fsnotify.Rename) != 0 {
					log.Log.Infof("backup tunnel socket %s removed or renamed (op=%s)", event.Name, event.Op)
					return
				}
			case err, ok := <-watcher.Errors:
				if !ok {
					return
				}
				log.Log.Reason(err).Error("backup tunnel fsnotify watcher error")
				return
			}
		}
	}()

	return ch, nil
}

type closeNotifyConn struct {
	net.Conn
	once   sync.Once
	closed chan struct{}
}

func (c *closeNotifyConn) Close() error {
	c.once.Do(func() { close(c.closed) })
	return c.Conn.Close()
}

// oneConnListener hands out exactly one connection on the first Accept call,
// then blocks subsequent Accept calls until the connection closes, at which
// point it returns io.EOF so gRPC's internal accept loop exits cleanly
type oneConnListener struct {
	mu     sync.Mutex
	conn   net.Conn
	closed chan struct{}
	addr   net.Addr
}

func (l *oneConnListener) Accept() (net.Conn, error) {
	l.mu.Lock()
	c := l.conn
	if c != nil {
		l.conn = nil
	}
	l.mu.Unlock()

	if c != nil {
		return c, nil
	}

	<-l.closed
	return nil, net.ErrClosed
}

func (l *oneConnListener) Close() error   { return nil }
func (l *oneConnListener) Addr() net.Addr { return l.addr }

func canCreateTunnel(targetAddr, serverName, nbdSocket, token string) error {
	switch {
	case targetAddr == "":
		return fmt.Errorf("targetAddr must not be empty")
	case serverName == "":
		return fmt.Errorf("serverName must not be empty")
	case nbdSocket == "":
		return fmt.Errorf("nbdSocket must not be empty")
	case token == "":
		return fmt.Errorf("token must not be empty")
	}

	return nil
}
