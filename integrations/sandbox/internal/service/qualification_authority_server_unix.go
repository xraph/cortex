//go:build linux || darwin

package service

import (
	"context"
	"net"
	"net/http"
	"net/netip"
	"os"
	"runtime"

	"github.com/xraph/cortex/evidence"
	broker "github.com/xraph/cortex/integrations/sandbox"
	"github.com/xraph/cortex/integrations/sandbox/internal/hostfs"
)

// QualificationAuthorityServerConfig pins the authority's listener and mTLS
// material. Policy changes take effect online; changing this file requires restart.
type QualificationAuthorityServerConfig struct {
	SchemaVersion int                          `json:"schema_version"`
	Authority     QualificationAuthorityConfig `json:"authority"`
	Listen        string                       `json:"listen"`
	Certificate   hostfs.Pin                   `json:"certificate"`
	TLSKey        hostfs.Pin                   `json:"tls_key"`
	ClientCAs     hostfs.Pin                   `json:"client_cas"`
	Limits        broker.ServerLimits          `json:"limits"`
}

// RunQualificationAuthority serves protected retained evidence on a Linux root
// host. Cancellation closes connections and joins handlers. It never runs probes.
func RunQualificationAuthority(ctx context.Context, name string) error {
	if runtime.GOOS != "linux" || os.Geteuid() != 0 {
		return errUnsupported
	}
	authority, server, address, err := readQualificationAuthority(ctx, name, 0)
	if err != nil {
		return err
	}
	listener, err := (&net.ListenConfig{}).Listen(ctx, "tcp", address)
	if err != nil {
		return broker.ErrDenied
	}
	return server.ServeHandler(ctx, listener, authority)
}
func readQualificationAuthority(ctx context.Context, name string, owner uint32) (*QualificationAuthority, *broker.Server, string, error) {
	body, err := hostfs.ReadOwned(ctx, name, owner, 0o600, 1<<20)
	if err != nil {
		return nil, nil, "", broker.ErrDenied
	}
	var cfg QualificationAuthorityServerConfig
	if decodeCanonical(body, &cfg) != nil || !validQualificationAuthorityServer(name, cfg) {
		return nil, nil, "", broker.ErrDenied
	}
	expected := evidence.Digest(body)
	trust := func(operation context.Context) error {
		current, readErr := hostfs.ReadOwned(operation, name, owner, 0o600, 1<<20)
		if readErr != nil || evidence.Digest(current) != expected {
			return broker.ErrDenied
		}
		for _, pin := range []hostfs.Pin{cfg.Certificate, cfg.TLSKey, cfg.ClientCAs} {
			if pinErr := hostfs.VerifyPin(operation, pin, owner); pinErr != nil {
				return broker.ErrDenied
			}
		}
		return operation.Err()
	}
	authority, err := newQualificationAuthority(cfg.Authority, owner, trust)
	if err != nil {
		return nil, nil, "", err
	}
	if _, _, err = authority.policy(ctx); err != nil {
		return nil, nil, "", err
	}
	certificate, pool, err := qualificationTLSMaterial(ctx, cfg.Certificate, cfg.TLSKey, cfg.ClientCAs, owner)
	if err != nil {
		return nil, nil, "", broker.ErrDenied
	}
	// ServeHandler owns authority authentication. Accidental use as a job broker
	// is denied by this callback, so evidence credentials cannot submit jobs.
	server, err := broker.NewServer(broker.ServerConfig{Certificate: certificate, ClientCAs: pool, Authenticate: func(*http.Request) (broker.Identity, error) { return broker.Identity{}, broker.ErrDenied }, Limits: cfg.Limits})
	if err != nil || trust(ctx) != nil {
		return nil, nil, "", broker.ErrDenied
	}
	return authority, server, cfg.Listen, nil
}
func validQualificationAuthorityServer(name string, cfg QualificationAuthorityServerConfig) bool {
	address, err := netip.ParseAddrPort(cfg.Listen)
	if err != nil || address.String() != cfg.Listen || address.Port() == 0 || address.Addr().Zone() != "" || address.Addr().IsMulticast() || cfg.SchemaVersion != 1 || cfg.Limits.Validate() != nil || cfg.Limits.RequestTimeout != cfg.Authority.Timeout {
		return false
	}
	if !validDaemonPin(cfg.Certificate, daemonCertificatePEMBytes) || !validDaemonPin(cfg.TLSKey, daemonTLSKeyPEMBytes) || !validDaemonPin(cfg.ClientCAs, daemonClientCAPEMBytes) {
		return false
	}
	seen := map[string]bool{}
	for _, path := range []string{name, cfg.Authority.Policy, cfg.Certificate.Path, cfg.TLSKey.Path, cfg.ClientCAs.Path} {
		if !profilePath(path) || seen[path] {
			return false
		}
		seen[path] = true
	}
	return true
}
