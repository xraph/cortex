//go:build linux || darwin

package service

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/tls"
	"crypto/x509"
	"encoding/asn1"
	"encoding/pem"
	"fmt"

	"github.com/xraph/cortex/evidence"
	broker "github.com/xraph/cortex/integrations/sandbox"
	"github.com/xraph/cortex/integrations/sandbox/internal/hostfs"
)

// DaemonCredentialFiles pins PEM files from protected daemon configuration.
// All four files must have mode 0600 and one link. Private keys use unencrypted
// PKCS8 PRIVATE KEY blocks; certificates use CERTIFICATE blocks. These pins and
// private material must never be included in worker configuration or credentials.
type DaemonCredentialFiles struct {
	SigningKey  hostfs.Pin `json:"signing_key"`
	Certificate hostfs.Pin `json:"certificate"`
	TLSKey      hostfs.Pin `json:"tls_key"`
	ClientCAs   hostfs.Pin `json:"client_cas"`
}

// DaemonCredentials binds a receipt-signing identity to an installed public
// server. The caller owns SigningKey. Server copies its TLS credentials and
// requires verified client certificates followed by current host authentication.
type DaemonCredentials struct {
	SigningKey ed25519.PrivateKey
	Server     *broker.Server
}

// ReadDaemonCredentials loads root-owned pinned credentials and constructs a
// bounded mTLS server without opening a listener. expected must come from the
// verified worker configuration. Identity mapping and current grants still belong
// to authenticate and the broker authorizer. Rotations require a new lifetime.
func ReadDaemonCredentials(ctx context.Context, files DaemonCredentialFiles, expected ed25519.PublicKey, authenticate broker.Authenticator, limits broker.ServerLimits) (*DaemonCredentials, error) {
	return readDaemonCredentials(ctx, files, expected, authenticate, limits, 0)
}

func readDaemonCredentials(ctx context.Context, files DaemonCredentialFiles, expected ed25519.PublicKey, authenticate broker.Authenticator, limits broker.ServerLimits, owner uint32) (*DaemonCredentials, error) {
	if len(expected) != ed25519.PublicKeySize || authenticate == nil {
		return nil, fmt.Errorf("cortex: missing daemon credential identity or authentication")
	}
	signingPEM, err := readDaemonPEM(ctx, files.SigningKey, 4<<10, owner)
	if err != nil {
		return nil, err
	}
	defer clear(signingPEM)
	blocks, err := credentialBlocks(signingPEM, "PRIVATE KEY", 1)
	if err != nil {
		return nil, err
	}
	defer clear(blocks[0])
	parsed, err := parseCredentialKey(blocks[0])
	if err != nil {
		return nil, fmt.Errorf("cortex: invalid receipt signing key: %w", err)
	}
	signing, ok := parsed.(ed25519.PrivateKey)
	if !ok {
		return nil, fmt.Errorf("cortex: receipt signing requires Ed25519")
	}
	returned := false
	defer func() {
		if !returned {
			clear(signing)
		}
	}()
	if !bytes.Equal(signing[ed25519.SeedSize:], expected) {
		return nil, broker.ErrConflict
	}
	certificatePEM, err := readDaemonPEM(ctx, files.Certificate, 192<<10, owner)
	if err != nil {
		return nil, err
	}
	chain, err := credentialBlocks(certificatePEM, "CERTIFICATE", 8)
	if err != nil {
		return nil, err
	}
	tlsPEM, err := readDaemonPEM(ctx, files.TLSKey, 24<<10, owner)
	if err != nil {
		return nil, err
	}
	defer clear(tlsPEM)
	keyBlocks, err := credentialBlocks(tlsPEM, "PRIVATE KEY", 1)
	if err != nil {
		return nil, err
	}
	defer clear(keyBlocks[0])
	tlsKey, err := parseCredentialKey(keyBlocks[0])
	if err != nil {
		return nil, fmt.Errorf("cortex: invalid TLS private key: %w", err)
	}
	caPEM, err := readDaemonPEM(ctx, files.ClientCAs, 256<<10, owner)
	if err != nil {
		return nil, err
	}
	caBlocks, err := credentialBlocks(caPEM, "CERTIFICATE", 32)
	if err != nil {
		return nil, err
	}
	pool := x509.NewCertPool()
	seen := make(map[string]bool, len(caBlocks))
	for _, der := range caBlocks {
		digest := evidence.Digest(der)
		ca, parseErr := x509.ParseCertificate(der)
		if parseErr != nil || seen[digest] || !ca.BasicConstraintsValid || !ca.IsCA || ca.KeyUsage&x509.KeyUsageCertSign == 0 {
			return nil, fmt.Errorf("cortex: invalid or duplicate client CA")
		}
		seen[digest] = true
		pool.AddCert(ca)
	}
	server, err := broker.NewServer(broker.ServerConfig{Certificate: tls.Certificate{Certificate: chain, PrivateKey: tlsKey}, ClientCAs: pool, Authenticate: authenticate, Limits: limits})
	if err != nil {
		return nil, err
	}
	if contextErr := ctx.Err(); contextErr != nil {
		return nil, contextErr
	}
	returned = true
	return &DaemonCredentials{SigningKey: signing, Server: server}, nil
}

func readDaemonPEM(ctx context.Context, pin hostfs.Pin, maximum int64, owner uint32) ([]byte, error) {
	if pin.Mode != 0o600 || pin.Bytes <= 0 || pin.Bytes > maximum {
		return nil, fmt.Errorf("cortex: invalid daemon credential pin")
	}
	return hostfs.ReadPin(ctx, pin, owner)
}

func parseCredentialKey(der []byte) (any, error) {
	var value asn1.RawValue
	rest, err := asn1.Unmarshal(der, &value)
	if err != nil || len(rest) != 0 {
		return nil, fmt.Errorf("cortex: invalid or trailing private key DER")
	}
	return x509.ParsePKCS8PrivateKey(der)
}

// credentialBlocks rejects PEM's permissive skipping of text and malformed
// blocks. Decode each first bounded block independently so a malformed prefix
// cannot be skipped in favor of a later valid credential.
func credentialBlocks(data []byte, kind string, maximum int) ([][]byte, error) {
	var blocks [][]byte
	begin, end := []byte("-----BEGIN "+kind+"-----"), []byte("-----END "+kind+"-----")
	for len(bytes.TrimSpace(data)) != 0 {
		data = bytes.TrimSpace(data)
		last := bytes.Index(data, end)
		if len(blocks) >= maximum || !bytes.HasPrefix(data, begin) || last < len(begin) {
			return nil, fmt.Errorf("cortex: invalid daemon credential PEM")
		}
		boundary := last + len(end)
		if bytes.Count(data[:boundary], []byte("-----BEGIN ")) != 1 {
			return nil, fmt.Errorf("cortex: nested daemon credential PEM")
		}
		block, rest := pem.Decode(data[:boundary])
		if block == nil || block.Type != kind || len(block.Headers) != 0 || len(bytes.TrimSpace(rest)) != 0 || len(block.Bytes) == 0 {
			return nil, fmt.Errorf("cortex: invalid daemon credential PEM block")
		}
		blocks = append(blocks, block.Bytes)
		data = data[boundary:]
	}
	if len(blocks) == 0 {
		return nil, fmt.Errorf("cortex: missing daemon credential PEM")
	}
	return blocks, nil
}
