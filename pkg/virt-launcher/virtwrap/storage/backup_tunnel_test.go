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
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"encoding/pem"
	"io"
	"net"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/go-jose/go-jose/v4"
	"github.com/go-jose/go-jose/v4/jwt"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	certutil "kubevirt.io/kubevirt/pkg/certificates/triple/cert"
)

var _ = Describe("Backup Tunnel", func() {
	Context("canCreateTunnel", func() {
		DescribeTable("should return an error when a required field is empty",
			func(targetAddr, serverName, nbdSocket, token string, expectedErrSubstr string) {
				err := canCreateTunnel(targetAddr, serverName, nbdSocket, token)
				Expect(err).To(HaveOccurred())
				Expect(err.Error()).To(ContainSubstring(expectedErrSubstr))
			},
			Entry("empty targetAddr", "", "server", "/tmp/nbd.sock", "token", "targetAddr"),
			Entry("empty serverName", "127.0.0.1", "", "/tmp/nbd.sock", "token", "serverName"),
			Entry("empty nbdSocket", "127.0.0.1", "server", "", "token", "nbdSocket"),
			Entry("empty token", "127.0.0.1", "server", "/tmp/nbd.sock", "", "token"),
		)

		It("returns nil when all fields are provided", func() {
			Expect(canCreateTunnel("127.0.0.1", "server", "/tmp/nbd.sock", "mytoken")).To(Succeed())
		})
	})

	Context("newBackupTunnelManager", func() {
		var (
			validCACert []byte
		)

		generateCACert := func() []byte {
			key, err := certutil.NewECDSAPrivateKey()
			Expect(err).ToNot(HaveOccurred())
			cert, err := certutil.NewSelfSignedCACert(certutil.Config{CommonName: "test"}, key, time.Hour)
			Expect(err).ToNot(HaveOccurred())
			var pemOut strings.Builder
			Expect(pem.Encode(&pemOut, &pem.Block{Type: "CERTIFICATE", Bytes: cert.Raw})).To(Succeed())
			return []byte(pemOut.String())
		}

		BeforeEach(func() {
			validCACert = generateCACert()
		})

		It("should create a manager successfully with a valid CA", func() {
			m, err := newBackupTunnelManager("127.0.0.1", "server", "/tmp/nbd.sock", "token", validCACert)
			Expect(err).ToNot(HaveOccurred())
			Expect(m).ToNot(BeNil())
			Expect(m.targetAddr).To(Equal("127.0.0.1"))
			Expect(m.nbdSocket).To(Equal("/tmp/nbd.sock"))
			Expect(m.token).To(Equal("token"))
			Expect(m.tlsConfig).ToNot(BeNil())
			Expect(m.tlsConfig.ServerName).To(Equal("server"))
			Expect(m.tlsConfig.InsecureSkipVerify).To(BeFalse())
		})

		It("returns an error when the CA cert PEM is invalid", func() {
			_, err := newBackupTunnelManager("127.0.0.1", "server", "/tmp/nbd.sock", "token", []byte("not-a-cert"))
			Expect(err).To(HaveOccurred())
			Expect(err.Error()).To(ContainSubstring("failed to parse CA certificate"))
		})

		It("returns an error when the CA cert PEM slice is empty", func() {
			_, err := newBackupTunnelManager("127.0.0.1", "server", "/tmp/nbd.sock", "token", []byte{})
			Expect(err).To(HaveOccurred())
			Expect(err.Error()).To(ContainSubstring("failed to parse CA certificate"))
		})
	})

	Context("closeNotifyConn", func() {
		It("should close the underlying connection and signals the channel exactly once", func() {
			server, client := net.Pipe()
			defer server.Close()

			closed := make(chan struct{})
			notify := &closeNotifyConn{Conn: client, closed: closed}

			Expect(notify.Close()).To(Succeed())

			Eventually(closed).Should(BeClosed())
			Expect(notify.Close()).To(Or(Succeed(), HaveOccurred()))
			Consistently(closed).Should(BeClosed())
		})
	})

	Context("oneConnListener", func() {
		It("should return the connection on the first Accept call", func() {
			_, client := net.Pipe()
			defer client.Close()

			closed := make(chan struct{})
			l := &oneConnListener{
				conn:   client,
				closed: closed,
				addr:   client.LocalAddr(),
			}

			conn, err := l.Accept()
			Expect(err).ToNot(HaveOccurred())
			Expect(conn).To(Equal(client))
		})

		It("should block on the second Accept until the closed channel is shut", func() {
			_, client := net.Pipe()
			defer client.Close()

			closed := make(chan struct{})
			l := &oneConnListener{
				conn:   client,
				closed: closed,
				addr:   client.LocalAddr(),
			}

			conn, err := l.Accept()
			Expect(err).ToNot(HaveOccurred())
			Expect(conn).ToNot(BeNil())

			result := make(chan error, 1)
			go func() {
				_, acceptErr := l.Accept()
				result <- acceptErr
			}()

			Consistently(result, 100*time.Millisecond).ShouldNot(Receive())

			close(closed)
			Eventually(result).Should(Receive(Equal(net.ErrClosed)))
		})

		It("should return the stored address", func() {
			_, client := net.Pipe()
			defer client.Close()

			addr := client.LocalAddr()
			l := &oneConnListener{addr: addr}
			Expect(l.Addr()).To(Equal(addr))
		})
	})

	Context("handshake", func() {
		var (
			manager *backupTunnelManager
		)

		BeforeEach(func() {
			manager = &backupTunnelManager{}
		})

		It("should succeed when the server responds with OK", func() {
			conn := &fakeConn{r: strings.NewReader("OK\r\n"), w: io.Discard}
			Expect(manager.handshake(conn)).To(Succeed())
		})

		It("should return an error when the server rejects the token", func() {
			conn := &fakeConn{r: strings.NewReader("bad response\r\n"), w: io.Discard}
			Expect(manager.handshake(conn)).To(MatchError(ContainSubstring("server rejected token")))
		})

		It("should return an error when the connection is closed before reading a response", func() {
			conn := &fakeConn{r: strings.NewReader(""), w: io.Discard}
			Expect(manager.handshake(conn)).To(HaveOccurred())
		})
	})

	Context("readUnbufferedLine", func() {
		It("should read exactly one line and leave remainder intact", func() {
			reader := strings.NewReader("OK\r\ngRPC PRI\r\n")
			line, err := readUnbufferedLine(reader)
			Expect(err).ToNot(HaveOccurred())
			Expect(line).To(Equal("OK"))

			remainder, err := io.ReadAll(reader)
			Expect(err).ToNot(HaveOccurred())
			Expect(string(remainder)).To(Equal("gRPC PRI\r\n"))
		})

		It("should fail if line is too long", func() {
			longStr := strings.Repeat("A", 2048) + "\n"
			reader := strings.NewReader(longStr)
			_, err := readUnbufferedLine(reader)
			Expect(err).To(HaveOccurred())
			Expect(err.Error()).To(ContainSubstring("limited to 1024 bytes"))
		})
	})

	Context("watchSocket", func() {
		var (
			manager  *backupTunnelManager
			sockPath string
		)

		BeforeEach(func() {
			tmpDir := GinkgoT().TempDir()
			sockPath = filepath.Join(tmpDir, "nbd.sock")
			manager = &backupTunnelManager{nbdSocket: sockPath}
		})

		It("should return a channel that is closed when the socket is removed", func() {
			f, err := os.Create(sockPath)
			Expect(err).ToNot(HaveOccurred())
			f.Close()
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()

			ch, err := manager.watchSocket(ctx)
			Expect(err).ToNot(HaveOccurred())
			Expect(os.Remove(sockPath)).To(Succeed())
			Eventually(ch, 5*time.Second).Should(BeClosed())
		})

		It("should return a channel that is closed when context is cancelled", func() {
			ctx, cancel := context.WithCancel(context.Background())

			ch, err := manager.watchSocket(ctx)
			Expect(err).ToNot(HaveOccurred())
			cancel()
			Eventually(ch, 3*time.Second).Should(BeClosed())
		})

		It("should return an error if the socket directory does not exist", func() {
			manager.nbdSocket = "/doesnotexist/nbd.sock"
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()

			_, err := manager.watchSocket(ctx)
			Expect(err).To(HaveOccurred())
			Expect(err.Error()).To(ContainSubstring("failed to watch directory"))
		})
	})

	Context("TTL handling", func() {
		generateToken := func(expiry *time.Time) string {
			key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
			Expect(err).ToNot(HaveOccurred())

			sig, err := jose.NewSigner(jose.SigningKey{Algorithm: jose.ES256, Key: key}, (&jose.SignerOptions{}).WithType("JWT"))
			Expect(err).ToNot(HaveOccurred())

			claims := jwt.Claims{}
			if expiry != nil {
				claims.Expiry = jwt.NewNumericDate(*expiry)
			}

			raw, err := jwt.Signed(sig).Claims(claims).Serialize()
			Expect(err).ToNot(HaveOccurred())
			return raw
		}

		It("should return an error if token is malformed", func() {
			_, _, err := extractContextFromToken("invalid.token.string")
			Expect(err).To(HaveOccurred())
			Expect(err.Error()).To(ContainSubstring("failed to parse JWT token"))
		})

		It("should return an error if token lacks an expiry claim", func() {
			token := generateToken(nil)
			_, _, err := extractContextFromToken(token)
			Expect(err).To(HaveOccurred())
			Expect(err.Error()).To(ContainSubstring("missing the required expiry claim"))
		})

		It("should return a context with the correct deadline", func() {
			expectedExpiry := time.Now().Add(1 * time.Hour).Truncate(time.Second)
			token := generateToken(&expectedExpiry)

			ctx, cancel, err := extractContextFromToken(token)
			Expect(err).ToNot(HaveOccurred())
			defer cancel()

			deadline, ok := ctx.Deadline()
			Expect(ok).To(BeTrue(), "context should have a deadline set")

			Expect(deadline).To(BeTemporally("==", expectedExpiry))
		})
	})
})

type fakeConn struct {
	net.Conn
	r io.Reader
	w io.Writer
}

func (f *fakeConn) Read(b []byte) (int, error)       { return f.r.Read(b) }
func (f *fakeConn) Write(b []byte) (int, error)      { return f.w.Write(b) }
func (f *fakeConn) SetReadDeadline(time.Time) error  { return nil }
func (f *fakeConn) SetWriteDeadline(time.Time) error { return nil }
