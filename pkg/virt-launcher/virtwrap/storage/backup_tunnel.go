package storage

import (
	"bufio"
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"io"
	"math"
	"net"
	"net/textproto"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/fsnotify/fsnotify"
	"google.golang.org/grpc"
	"google.golang.org/grpc/keepalive"
	"gopkg.in/go-jose/go-jose.v2/jwt"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/utils/clock"

	"kubevirt.io/client-go/log"

	nbdv1 "kubevirt.io/kubevirt/pkg/nbd-com/nbd/v1"
)

const (
	defaultDialTimeout      = 10 * time.Second
	defaultHandshakeTimeout = 10 * time.Second
	defaultExportServerPort = 9090

	// gRPC keepalive — detects half-open TCP connections and prevents idle
	// connections from being silently dropped by intermediate NAT/firewalls.
	grpcKeepaliveTime    = 15 * time.Second
	grpcKeepaliveTimeout = 5 * time.Second
	grpcKeepaliveIdle    = 30 * time.Second

	// grpcMaxMsgSize is the larger of the two streaming message types:
	//   - DataChunk:   maxReadChunkSize payload + ~15 bytes protobuf/gRPC framing (~256 KiB)
	//   - MapResponse: mapResponseBatchSize extents × ~41 bytes per extent        (~20 KiB)
	// DataChunk dominates. 1 KiB headroom covers all framing overhead.
	grpcMaxMsgSize = maxReadChunkSize + 1024

	// Upper bound on what we'll read from the server during the handshake.
	// The server sends "OK" or a short error string; 512 bytes is ample and
	// prevents a misbehaving peer from allocating unbounded memory on our side.
	maxHandshakeResponseBytes = 512

	// Exponential backoff parameters for reconnects.
	// Steps=math.MaxInt32 is effectively unlimited; the socket removal or
	// Stop() call are the true termination conditions.
	backoffInitial = 1 * time.Second
	backoffCap     = 60 * time.Second
	backoffFactor  = 2.0
	backoffJitter  = 0.2
	backoffSteps   = math.MaxInt32

	// If a connection was healthy for longer than backoffResetDuration, the
	// backoff resets to backoffInitial on the next failure. This ensures a
	// brief outage after a long stable connection retries quickly rather than
	// starting from wherever the exponential sequence previously left off.
	backoffResetDuration = 2 * backoffCap
)

type backupTunnelManager struct {
	targetAddr       string
	nbdSocket        string
	token            string
	tlsConfig        *tls.Config
	serverPort       int
	dialTimeout      time.Duration
	handshakeTimeout time.Duration
	tunnelExpiry     time.Time

	// mutable: guarded by mu
	mu     sync.Mutex
	server *grpc.Server
}

func newBackupTunnelManager(
	targetAddr, serverName, nbdSocket, token string,
	caCert []byte,
) (*backupTunnelManager, error) {
	switch {
	case targetAddr == "":
		return nil, errors.New("targetAddr must not be empty")
	case serverName == "":
		return nil, errors.New("serverName must not be empty")
	case nbdSocket == "":
		return nil, errors.New("nbdSocket must not be empty")
	case token == "":
		return nil, errors.New("token must not be empty")
	}

	certPool := x509.NewCertPool()
	if ok := certPool.AppendCertsFromPEM(caCert); !ok {
		return nil, errors.New("failed to parse CA certificate: no valid PEM blocks found")
	}

	expiry, err := jwtExpiry(token)
	if err != nil {
		return nil, fmt.Errorf("invalid token: %w", err)
	}

	if time.Now().After(expiry) {
		return nil, fmt.Errorf("token already expired at %s", expiry)
	}

	return &backupTunnelManager{
		targetAddr:       targetAddr,
		nbdSocket:        nbdSocket,
		token:            token,
		serverPort:       defaultExportServerPort,
		dialTimeout:      defaultDialTimeout,
		handshakeTimeout: defaultHandshakeTimeout,
		tlsConfig: &tls.Config{
			RootCAs:            certPool,
			ServerName:         serverName,
			InsecureSkipVerify: false,
		},
		tunnelExpiry: expiry,
	}, nil
}

func (m *backupTunnelManager) Run() {
	ctx, cancel := context.WithDeadlineCause(
		context.Background(),
		m.tunnelExpiry,
		fmt.Errorf("backup tunnel token has expired: %s", m.tunnelExpiry),
	)
	defer cancel()
	socketGone := m.watchSocket(ctx)

	delayFn := wait.Backoff{
		Duration: backoffInitial,
		Cap:      backoffCap,
		Factor:   backoffFactor,
		Jitter:   backoffJitter,
		Steps:    backoffSteps,
	}.DelayWithReset(&clock.RealClock{}, backoffResetDuration)

	err := delayFn.Until(ctx, true, true, func(ctx context.Context) (bool, error) {
		select {
		case <-socketGone:
			log.Log.Infof("backup tunnel: NBD socket %s removed — stopping", m.nbdSocket)
			return false, fmt.Errorf("NBD socket removed")
		default:
		}
		if _, err := os.Stat(m.nbdSocket); errors.Is(err, os.ErrNotExist) {
			log.Log.Infof("backup tunnel: NBD socket %s not found — stopping", m.nbdSocket)
			return false, fmt.Errorf("NBD socket not found")
		}

		if err := m.establishAndServe(ctx, socketGone); err != nil {
			log.Log.Reason(err).Warning("backup tunnel: connection lost, will retry")
			return false, nil
		}

		return true, nil // clean exit
	})

	if err != nil && !errors.Is(err, context.Canceled) && !errors.Is(err, context.DeadlineExceeded) {
		log.Log.Reason(err).Error("backup tunnel: stopped with terminal error")
	}

	m.stopServer()
}

func (m *backupTunnelManager) Stop() {
	m.stopServer()
}

func (m *backupTunnelManager) establishAndServe(ctx context.Context, socketGone <-chan struct{}) error {
	addr := fmt.Sprintf("%s:%d", m.targetAddr, m.serverPort)

	// 1. Dial with explicit timeout.
	conn, err := m.dial(addr)
	if err != nil {
		return fmt.Errorf("dial %s: %w", addr, err)
	}
	// conn is owned by the gRPC server once Serve() is called.
	// On handshake failure we close it explicitly and return before reaching Serve().

	// 2. Send JWT and verify server acknowledgement.
	if err := m.handshake(conn); err != nil {
		conn.Close()
		return fmt.Errorf("handshake: %w", err)
	}

	// 3. Build the gRPC server with production-safe defaults.
	srv := grpc.NewServer(
		grpc.KeepaliveParams(keepalive.ServerParameters{
			MaxConnectionIdle: grpcKeepaliveIdle,
			Time:              grpcKeepaliveTime,
			Timeout:           grpcKeepaliveTimeout,
		}),
		grpc.KeepaliveEnforcementPolicy(keepalive.EnforcementPolicy{
			MinTime:             5 * time.Second,
			PermitWithoutStream: true,
		}),
	)
	nbdv1.RegisterNBDServer(srv, NewNBDClient(m.nbdSocket))

	m.mu.Lock()
	m.server = srv
	m.mu.Unlock()

	log.Log.Infof("backup tunnel: connected to %s, serving NBD over gRPC", addr)

	// 4. Graceful-shutdown goroutine: drains in-flight RPCs on context
	//    cancellation or socket removal, with a hard-stop timeout fallback.
	go func() {
		select {
		case <-ctx.Done():
			log.Log.Info("backup tunnel: context done — draining gRPC server")
		case <-socketGone:
			log.Log.Info("backup tunnel: NBD socket gone — draining gRPC server")
		}

		gracefulDone := make(chan struct{})
		go func() {
			defer close(gracefulDone)
			srv.GracefulStop()
		}()

		select {
		case <-gracefulDone:
		case <-time.After(10 * time.Second):
			log.Log.Warning("backup tunnel: graceful drain timed out — forcing stop")
			srv.Stop()
		}
	}()

	// 5. Serve blocks until the gRPC server stops.
	closed := make(chan struct{})
	wrapped := &closeNotifyConn{Conn: conn, closed: closed}
	if err := srv.Serve(&oneConnListener{
		conn:   wrapped,
		closed: closed,
		addr:   conn.LocalAddr(),
	}); err != nil && !isExpectedServeError(err) {
		return err
	}

	m.mu.Lock()
	m.server = nil
	m.mu.Unlock()

	select {
	case <-ctx.Done():
		return nil
	case <-socketGone:
		return nil
	default:
		return fmt.Errorf("remote closed connection unexpectedly")
	}
}

func (m *backupTunnelManager) dial(addr string) (*tls.Conn, error) {
	return tls.DialWithDialer(&net.Dialer{Timeout: m.dialTimeout}, "tcp", addr, m.tlsConfig)
}

func (m *backupTunnelManager) handshake(conn net.Conn) error {
	deadline := time.Now().Add(m.handshakeTimeout)

	// Write deadline prevents a slow/unresponsive peer from hanging the send.
	if err := conn.SetWriteDeadline(deadline); err != nil {
		return fmt.Errorf("set write deadline: %w", err)
	}
	if _, err := fmt.Fprintf(conn, "%s\n", m.token); err != nil {
		return fmt.Errorf("send token: %w", err)
	}
	conn.SetWriteDeadline(time.Time{})

	// Read deadline bounds how long we wait for the server's "OK".
	if err := conn.SetReadDeadline(deadline); err != nil {
		return fmt.Errorf("set read deadline: %w", err)
	}
	// textproto.Reader.ReadLine is the stdlib primitive for line-based text
	// protocols (used by net/http, net/smtp, etc.). io.LimitReader caps
	// allocation to maxHandshakeResponseBytes regardless of what the peer sends.
	resp, err := textproto.NewReader(
		bufio.NewReader(io.LimitReader(conn, maxHandshakeResponseBytes)),
	).ReadLine()
	if err != nil {
		return fmt.Errorf("read response: %w", err)
	}
	conn.SetReadDeadline(time.Time{})

	if resp != "OK" {
		return fmt.Errorf("server rejected token: %q", resp)
	}
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

// ---------------------------------------------------------------------------
// Socket watcher
// ---------------------------------------------------------------------------

// watchSocket returns a channel that is closed when the NBD socket is removed
// or renamed. If fsnotify is unavailable it logs and falls back gracefully —
// Run's os.Stat check remains as a safety net.
func (m *backupTunnelManager) watchSocket(ctx context.Context) <-chan struct{} {
	ch := make(chan struct{})

	go func() {
		defer close(ch)

		watcher, err := fsnotify.NewWatcher()
		if err != nil {
			log.Log.Reason(err).Error("backup tunnel: fsnotify unavailable — reactive socket teardown disabled")
			<-ctx.Done()
			return
		}
		defer watcher.Close()

		if err := watcher.Add(filepath.Dir(m.nbdSocket)); err != nil {
			log.Log.Reason(err).Errorf("backup tunnel: cannot watch socket directory — reactive teardown disabled")
			<-ctx.Done()
			return
		}

		for {
			select {
			case <-ctx.Done():
				return
			case event, ok := <-watcher.Events:
				if !ok {
					return
				}
				if event.Name == m.nbdSocket && event.Op&(fsnotify.Remove|fsnotify.Rename) != 0 {
					log.Log.Infof("backup tunnel: socket %s removed (op=%s)", event.Name, event.Op)
					return // closing ch signals Run and establishAndServe
				}
			case err, ok := <-watcher.Errors:
				if !ok {
					return
				}
				log.Log.Reason(err).Warning("backup tunnel: fsnotify error")
			}
		}
	}()

	return ch
}

// ---------------------------------------------------------------------------
// Helpers
// ---------------------------------------------------------------------------

func isExpectedServeError(err error) bool {
	if err == nil || errors.Is(err, io.EOF) || errors.Is(err, net.ErrClosed) {
		return true
	}
	// gRPC does not export a sentinel for "server was stopped"; match the
	// string it consistently produces. Keep this in one place so it's easy
	// to update if upstream ever adds a typed error.
	const grpcServerStopped = "use of closed network connection"
	return errors.Is(err, net.ErrClosed) || strings.Contains(err.Error(), grpcServerStopped)
}

// ---------------------------------------------------------------------------
// oneConnListener — adapts a single net.Conn into a net.Listener for gRPC.
// ---------------------------------------------------------------------------

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
// point it returns io.EOF so gRPC's internal accept loop exits cleanly.
type oneConnListener struct {
	mu     sync.Mutex
	conn   net.Conn // nil after the first Accept
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
	return nil, io.EOF
}

func (l *oneConnListener) Close() error   { return nil }
func (l *oneConnListener) Addr() net.Addr { return l.addr }

func jwtExpiry(tokenStr string) (time.Time, error) {
	tok, err := jwt.ParseSigned(tokenStr)
	if err != nil {
		return time.Time{}, fmt.Errorf("parse token: %w", err)
	}
	claims := jwt.Claims{}
	if err := tok.UnsafeClaimsWithoutVerification(&claims); err != nil {
		return time.Time{}, fmt.Errorf("decode claims: %w", err)
	}
	if claims.Expiry == nil {
		return time.Time{}, fmt.Errorf("token has no expiry claim")
	}
	return claims.Expiry.Time(), nil
}
