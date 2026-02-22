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

package cbt

import (
	"crypto/ecdsa"
	"crypto/rand"
	"crypto/rsa"
	"crypto/tls"
	"time"

	"github.com/go-jose/go-jose/v4"
	"github.com/go-jose/go-jose/v4/jwt"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"kubevirt.io/kubevirt/pkg/certificates/bootstrap"
)

var _ = Describe("Pull token generator", func() {
	var (
		generator  *tokenGenerator
		expiration time.Time
	)

	BeforeEach(func() {
		fakeCertManager, err := bootstrap.NewMockCertificateManager()
		Expect(err).ToNot(HaveOccurred())
		generator = newTokenGenerator(fakeCertManager)
		expiration = time.Now().Truncate(time.Second).Add(time.Hour)
	})

	It("should generate a correct pull backup token JWT", func() {
		backupUID := "my-backup-uid"

		token, err := generator.generate(backupUID, expiration)
		Expect(err).ToNot(HaveOccurred())

		claims := mustParseClaims(generator, token)
		Expect(claims.Issuer).To(Equal(backupTokenIssuer))
		Expect(claims.Subject).To(Equal(backupUID))
		Expect(claims.Audience).To(ConsistOf(backupUID))
	})

	It("should generate a token with an expiry matching the requested TTL", func() {
		token, err := generator.generate("uid-ttl-test", expiration)
		Expect(err).ToNot(HaveOccurred())

		claims := mustParseClaims(generator, token)
		Expect(claims.Expiry.Time()).To(BeTemporally("==", expiration))
	})

	It("should generate a token that is not yet expired", func() {
		token, err := generator.generate("uid-expiry", expiration)
		Expect(err).ToNot(HaveOccurred())

		claims := mustParseClaims(generator, token)
		Expect(claims.ValidateWithLeeway(jwt.Expected{Time: time.Now()}, 0)).To(Succeed())
	})

	It("should generate distinct tokens for different backup UIDs", func() {
		token1, err := generator.generate("uid-one", expiration)
		Expect(err).ToNot(HaveOccurred())

		token2, err := generator.generate("uid-two", expiration)
		Expect(err).ToNot(HaveOccurred())

		Expect(token1).ToNot(Equal(token2))
	})

	It("should return an error when the cert manager has no certificate", func() {
		generator.certManager = &emptyCertManager{}
		_, err := generator.generate("uid-no-cert", expiration)
		Expect(err).To(HaveOccurred())
		Expect(err.Error()).To(ContainSubstring("no certificate available"))
	})

	It("should return an error when the certificate key is not ECDSA", func() {
		generator.certManager = &nonECDSACertManager{}
		_, err := generator.generate("uid-wrong-key", expiration)
		Expect(err).To(HaveOccurred())
		Expect(err.Error()).To(ContainSubstring("not an ECDSA private key"))
	})
})

func mustParseClaims(g *tokenGenerator, token string) jwt.Claims {
	parsed, err := jwt.ParseSigned(token, []jose.SignatureAlgorithm{jose.ES256, jose.RS256})
	Expect(err).ToNot(HaveOccurred())

	tlsCert := g.certManager.Current()
	Expect(tlsCert).ToNot(BeNil())
	ecKey, ok := tlsCert.PrivateKey.(*ecdsa.PrivateKey)
	Expect(ok).To(BeTrue())

	var claims jwt.Claims
	Expect(parsed.Claims(&ecKey.PublicKey, &claims)).To(Succeed())
	return claims
}

type emptyCertManager struct{}

func (e *emptyCertManager) Start()                    {}
func (e *emptyCertManager) Stop()                     {}
func (e *emptyCertManager) Current() *tls.Certificate { return nil }
func (e *emptyCertManager) ServerHealthy() bool       { return false }

type nonECDSACertManager struct{}

func (n *nonECDSACertManager) Start() {}
func (n *nonECDSACertManager) Stop()  {}
func (n *nonECDSACertManager) Current() *tls.Certificate {
	rsaKey, err := rsa.GenerateKey(rand.Reader, 2048)
	Expect(err).ToNot(HaveOccurred())
	return &tls.Certificate{PrivateKey: rsaKey}
}
func (n *nonECDSACertManager) ServerHealthy() bool { return true }
