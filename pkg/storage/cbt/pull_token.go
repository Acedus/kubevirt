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
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	jose "gopkg.in/go-jose/go-jose.v2"
	"gopkg.in/go-jose/go-jose.v2/jwt"

	"kubevirt.io/kubevirt/pkg/certificates/triple/cert"

	corev1 "k8s.io/api/core/v1"
)

const (
	backupTokenIssuer  = "kubevirt-backup-controller"
	backupSigningCerts = "/etc/virt-controller/backupcertificates"
)

type tokenGenerator struct {
	privKey *ecdsa.PrivateKey
	issuer  string
}

func newTokenGenerator(key *ecdsa.PrivateKey) *tokenGenerator {
	return &tokenGenerator{
		privKey: key,
		issuer:  backupTokenIssuer,
	}
}

func (g *tokenGenerator) Generate(backupUID string) (string, error) {
	sig, err := jose.NewSigner(
		jose.SigningKey{Algorithm: jose.ES256, Key: g.privKey},
		(&jose.SignerOptions{}).WithHeader("typ", "JWT"),
	)
	if err != nil {
		return "", err
	}

	cl := jwt.Claims{
		Issuer:   g.issuer,
		Subject:  backupUID,
		Audience: jwt.Audience{backupUID},
		Expiry:   jwt.NewNumericDate(time.Now().Add(10 * time.Minute)),
	}

	return jwt.Signed(sig).Claims(cl).CompactSerialize()
}

func backupPrivateKey() (*ecdsa.PrivateKey, error) {
	keyPath := filepath.Join(backupSigningCerts, corev1.TLSPrivateKeyKey)

	keyBytes, err := os.ReadFile(keyPath)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, fmt.Errorf("backup signing key not found at %s: ensure secret is populated", keyPath)
		}
		return nil, fmt.Errorf("failed to read backup signing key: %v", err)
	}

	parsedKey, err := cert.ParsePrivateKeyPEM(keyBytes)
	if err != nil {
		return nil, err
	}

	privateKey, ok := parsedKey.(*ecdsa.PrivateKey)
	if !ok {
		return nil, fmt.Errorf("data doesn't contain valid ECDSA Private Key")
	}

	return privateKey, nil
}
