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
	"fmt"
	"time"

	jose "github.com/go-jose/go-jose/v4"
	"github.com/go-jose/go-jose/v4/jwt"
	"k8s.io/client-go/util/certificate"
)

const (
	backupTokenIssuer = "kubevirt-backup-controller"
)

type tokenGenerator struct {
	certManager certificate.Manager
	issuer      string
}

func newTokenGenerator(certManager certificate.Manager) *tokenGenerator {
	return &tokenGenerator{
		certManager: certManager,
		issuer:      backupTokenIssuer,
	}
}

func (g *tokenGenerator) generate(backupUID string, expiration time.Time) (string, error) {
	key, err := ecdsaKeyFromManager(g.certManager)
	if err != nil {
		return "", fmt.Errorf("tokenGenerator: could not obtain signing key: %w", err)
	}

	sig, err := jose.NewSigner(
		jose.SigningKey{Algorithm: jose.ES256, Key: key},
		(&jose.SignerOptions{}).WithHeader("typ", "JWT"),
	)
	if err != nil {
		return "", fmt.Errorf("tokenGenerator: failed to create signer: %w", err)
	}

	now := time.Now()
	cl := jwt.Claims{
		Issuer:    g.issuer,
		Subject:   backupUID,
		Audience:  jwt.Audience{backupUID},
		IssuedAt:  jwt.NewNumericDate(now),
		NotBefore: jwt.NewNumericDate(now),
		Expiry:    jwt.NewNumericDate(expiration),
	}

	token, err := jwt.Signed(sig).Claims(cl).Serialize()
	if err != nil {
		return "", fmt.Errorf("tokenGenerator: serialization failed: %w", err)
	}

	return token, nil
}

func ecdsaKeyFromManager(m certificate.Manager) (*ecdsa.PrivateKey, error) {
	tlsCert := m.Current()
	if tlsCert == nil {
		return nil, fmt.Errorf("no certificate available")
	}
	// TODO: potentially support more algorithms in the future
	ecKey, ok := tlsCert.PrivateKey.(*ecdsa.PrivateKey)
	if !ok {
		return nil, fmt.Errorf("signing key is not an ECDSA private key (got %T)", tlsCert.PrivateKey)
	}
	return ecKey, nil
}
