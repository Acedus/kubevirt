package storage

import (
	"bufio"
	"context"
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"io"
	"net"
	"sync"
	"time"

	"google.golang.org/grpc"

	"kubevirt.io/client-go/log"

	nbdv1 "kubevirt.io/kubevirt/pkg/nbd-com/nbd/v1"
)

const (
	retryDelay  = 5 * time.Second
	dialTimeout = 10 * time.Second
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

func NewBackupTunnelManager(targetAddr, nbdSocket string, caCert []byte, token string) (*BackupTunnelManager, error) {
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
	for {
		select {
		case <-m.ctx.Done():
			return
		default:
			if err := m.establishAndServe(); err != nil {
				log.Log.Reason(err).Error("Backup tunnel connection lost, retrying...")
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
	conn, err := tls.DialWithDialer(dialer, "tcp", m.targetAddr, m.tlsConfig)
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
