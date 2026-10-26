//go:build linux || darwin

package service

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"net"
	"net/http"
	"net/url"
	"time"

	"github.com/xraph/cortex/evidence"
	broker "github.com/xraph/cortex/integrations/sandbox"
)

func qualificationTransport(ctx context.Context, cfg QualificationClientConfig, owner uint32) (*http.Transport, error) {
	certificatePEM, err := readDaemonPEM(ctx, cfg.Certificate, daemonCertificatePEMBytes, owner)
	if err != nil {
		return nil, err
	}
	if _, err = credentialBlocks(certificatePEM, "CERTIFICATE", 8); err != nil {
		return nil, err
	}
	keyPEM, err := readDaemonPEM(ctx, cfg.TLSKey, daemonTLSKeyPEMBytes, owner)
	if err != nil {
		return nil, err
	}
	defer clear(keyPEM)
	blocks, err := credentialBlocks(keyPEM, "PRIVATE KEY", 1)
	if err != nil {
		return nil, err
	}
	defer clear(blocks[0])
	if _, err = parseCredentialKey(blocks[0]); err != nil {
		return nil, broker.ErrDenied
	}
	certificate, err := tls.X509KeyPair(certificatePEM, keyPEM)
	if err != nil {
		return nil, broker.ErrDenied
	}
	caPEM, err := readDaemonPEM(ctx, cfg.ServerCAs, daemonClientCAPEMBytes, owner)
	if err != nil {
		return nil, err
	}
	authorities, err := credentialBlocks(caPEM, "CERTIFICATE", 32)
	if err != nil {
		return nil, err
	}
	pool, seen := x509.NewCertPool(), make(map[string]bool)
	for _, der := range authorities {
		authority, parseErr := x509.ParseCertificate(der)
		digest := evidence.Digest(der)
		if parseErr != nil || seen[digest] || !authority.BasicConstraintsValid || !authority.IsCA || authority.KeyUsage&x509.KeyUsageCertSign == 0 {
			return nil, broker.ErrDenied
		}
		seen[digest] = true
		pool.AddCert(authority)
	}
	endpoint, err := url.Parse(cfg.Endpoint)
	if err != nil {
		return nil, broker.ErrDenied
	}
	secure := &tls.Config{MinVersion: tls.VersionTLS13, RootCAs: pool, ServerName: endpoint.Hostname(), Certificates: []tls.Certificate{certificate}, VerifyConnection: func(state tls.ConnectionState) error {
		if len(state.VerifiedChains) == 0 || len(state.PeerCertificates) == 0 || evidence.Digest(state.PeerCertificates[0].RawSubjectPublicKeyInfo) != cfg.ServerSPKI {
			return broker.ErrDenied
		}
		return nil
	}}
	if contextErr := ctx.Err(); contextErr != nil {
		return nil, contextErr
	}
	// A fresh handshake rechecks certificate validity on every online decision.
	return &http.Transport{Proxy: nil, DialContext: (&net.Dialer{Timeout: cfg.Timeout}).DialContext, TLSClientConfig: secure, TLSHandshakeTimeout: cfg.Timeout, ResponseHeaderTimeout: cfg.Timeout, MaxResponseHeaderBytes: 8192, DisableKeepAlives: true, DisableCompression: true, MaxConnsPerHost: cfg.Readers, IdleConnTimeout: time.Second}, nil
}
