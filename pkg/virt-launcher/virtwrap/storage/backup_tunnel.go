package storage

import (
	"bufio"
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/fsnotify/fsnotify"
	"google.golang.org/grpc"

	"kubevirt.io/client-go/log"

	nbdv1 "kubevirt.io/kubevirt/pkg/nbd-com/nbd/v1"
)

const (
	retryDelay       = 5 * time.Second
	dialTimeout      = 10 * time.Second
	exportServerPort = 9090
)

type BackupTunnelManager struct {
	targetAddr string
	nbdSocket  string
	tlsConfig  *tls.Config
	token      string

	ctx    context.Context
	cancel context.CancelFunc

	mu     sync.Mutex
	server *grpc.Server
}

func newBackupTunnelManager(targetAddr, nbdSocket string, caCert []byte, token string) (*BackupTunnelManager, error) {
	certPool := x509.NewCertPool()
	if ok := certPool.AppendCertsFromPEM(caCert); !ok {
		return nil, fmt.Errorf("failed to parse CA certificate")
	}

	ctx, cancel := context.WithCancel(context.Background())

	return &BackupTunnelManager{
		ctx:        ctx,
		cancel:     cancel,
		targetAddr: targetAddr,
		nbdSocket:  nbdSocket,
		token:      token,
		tlsConfig: &tls.Config{
			RootCAs:            certPool,
			InsecureSkipVerify: true,
		},
	}, nil
}

func (m *BackupTunnelManager) Run() {
	go m.watchSocketRemoval()

	for {
		select {
		case <-m.ctx.Done():
			return
		default:
			if _, err := os.Stat(m.nbdSocket); errors.Is(err, os.ErrNotExist) {
				log.Log.Infof("backup socket %s does not exist. terminating tunnel.", m.nbdSocket)
				m.Stop()
				return
			}
			if err := m.establishAndServe(); err != nil {
				log.Log.Reason(err).Error("backup tunnel connection lost, retrying...")
				select {
				case <-time.After(retryDelay):
				case <-m.ctx.Done():
					return
				}
			}
		}
	}
}

func (m *BackupTunnelManager) establishAndServe() error {
	dialer := &net.Dialer{Timeout: dialTimeout}
	conn, err := tls.DialWithDialer(dialer, "tcp", fmt.Sprintf("%s:%d", m.targetAddr, exportServerPort), m.tlsConfig)
	if err != nil {
		return err
	}
	defer conn.Close()

	fmt.Fprintf(conn, "%s\n", m.token)
	resp, err := bufio.NewReader(conn).ReadString('\n')
	if err != nil || resp != "OK\n" {
		return fmt.Errorf("handshake failed: %v", err)
	}

	m.mu.Lock()
	m.server = grpc.NewServer()
	m.mu.Unlock()

	nbdv1.RegisterNBDServer(m.server, NewNBDClient(m.nbdSocket))

	go func() {
		<-m.ctx.Done()
		m.mu.Lock()
		if m.server != nil {
			m.server.GracefulStop()
		}
		m.mu.Unlock()
	}()

	err = m.server.Serve(&oneConnListener{conn: conn})
	if err != nil && err != io.EOF {
		return err
	}
	return nil
}

func (m *BackupTunnelManager) Stop() {
	m.cancel()

	m.mu.Lock()
	if m.server != nil {
		m.server.Stop()
	}
	m.mu.Unlock()
}

func (m *BackupTunnelManager) watchSocketRemoval() {
	watcher, err := fsnotify.NewWatcher()
	if err != nil {
		log.Log.Reason(err).Error("failed to create fsnotify watcher")
		return
	}
	defer watcher.Close()

	dir := filepath.Dir(m.nbdSocket)
	if err := watcher.Add(dir); err != nil {
		log.Log.Reason(err).Errorf("failed to watch backup socket directory: %s", dir)
		return
	}

	for {
		select {
		case <-m.ctx.Done():
			return
		case event, ok := <-watcher.Events:
			if !ok {
				return
			}
			if event.Name == m.nbdSocket && (event.Op&fsnotify.Remove == fsnotify.Remove || event.Op&fsnotify.Rename == fsnotify.Rename) {
				log.Log.Infof("reactive teardown: %s removed. Stopping tunnel.", event.Name)
				m.Stop()
				return
			}
		case err, ok := <-watcher.Errors:
			if !ok {
				return
			}
			log.Log.Reason(err).Error("fsnotify error")
		}
	}
}

type oneConnListener struct {
	conn net.Conn
	once sync.Once
}

func (l *oneConnListener) Accept() (net.Conn, error) {
	var c net.Conn
	l.once.Do(func() { c = l.conn })
	if c != nil {
		return c, nil
	}
	return nil, io.EOF
}

func (l *oneConnListener) Close() error   { return nil }
func (l *oneConnListener) Addr() net.Addr { return l.conn.LocalAddr() }
