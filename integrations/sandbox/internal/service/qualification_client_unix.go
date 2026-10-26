//go:build linux || darwin

package service

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/url"
	"os"
	"runtime"
	"slices"
	"strings"
	"time"

	"github.com/xraph/cortex/evidence"
	broker "github.com/xraph/cortex/integrations/sandbox"
	"github.com/xraph/cortex/integrations/sandbox/internal/hostfs"
)

const qualificationResponseBytes = 64 << 10

// QualificationClientConfig pins a separately provisioned authority and its
// mTLS credentials. You must install its canonical JSON as root with mode 0600.
// The authority owns live evidence verification; these settings grant no result.
type QualificationClientConfig struct {
	SchemaVersion  int           `json:"schema_version"`
	Endpoint       string        `json:"endpoint"`
	Source         string        `json:"source"`
	ServerSPKI     string        `json:"server_spki"`
	Certificate    hostfs.Pin    `json:"certificate"`
	TLSKey         hostfs.Pin    `json:"tls_key"`
	ServerCAs      hostfs.Pin    `json:"server_cas"`
	Timeout        time.Duration `json:"timeout_ns"`
	MaxEvidenceAge time.Duration `json:"max_evidence_age_ns"`
	Readers        int           `json:"readers"`
}

// QualificationHost identifies the current Linux installation and boot. MachineID
// is the SHA256 of /etc/machine-id; the kernel fields come from uname.
type QualificationHost struct {
	MachineID     string `json:"machine_id"`
	BootID        string `json:"boot_id"`
	KernelRelease string `json:"kernel_release"`
	KernelVersion string `json:"kernel_version"`
	Architecture  string `json:"architecture"`
}

// QualificationRequest asks the authority to revalidate retained live evidence
// and current revocation for one deployment. Nonce is fresh for every request.
type QualificationRequest struct {
	SchemaVersion int                `json:"schema_version"`
	Source        string             `json:"source"`
	Nonce         string             `json:"nonce"`
	Host          QualificationHost  `json:"host"`
	Deployment    DeploymentIdentity `json:"deployment"`
}

// QualificationEvidence references the authority's retained live probe record.
// Checks names every required passing probe group. ControlsDigest binds the
// effective controls checked by those probes to the installed profile.
type QualificationEvidence struct {
	Kind           string    `json:"kind"`
	Reference      string    `json:"reference"`
	SHA256         string    `json:"sha256"`
	ObservedAt     time.Time `json:"observed_at"`
	ControlsDigest string    `json:"controls_digest"`
	Checks         []string  `json:"checks"`
}

// QualificationResponse is an online authority decision. CheckedAt records a
// fresh evidence and revocation check after receiving Nonce. Only qualified
// decisions with complete live evidence may contain usable capabilities.
type QualificationResponse struct {
	SchemaVersion int                     `json:"schema_version"`
	Source        string                  `json:"source"`
	Nonce         string                  `json:"nonce"`
	Host          QualificationHost       `json:"host"`
	Status        string                  `json:"status"`
	CheckedAt     time.Time               `json:"checked_at"`
	ValidUntil    time.Time               `json:"valid_until"`
	Qualification DeploymentQualification `json:"qualification"`
	Evidence      QualificationEvidence   `json:"evidence"`
}

// QualificationClient consults one pinned authority without caching decisions.
// Verify supports concurrent callers up to the configured bound. Close releases
// idle connections after the owning daemon has joined its active operations.
type QualificationClient struct {
	cfg       QualificationClientConfig
	http      *http.Client
	transport *http.Transport
	trust     func(context.Context) error
	observe   func(context.Context) (QualificationHost, error)
	slots     chan struct{}
}

// OpenQualificationClient loads protected trust settings on a Linux root host.
// It makes no network request and cannot qualify an installation by itself.
func OpenQualificationClient(ctx context.Context, name string) (*QualificationClient, error) {
	if runtime.GOOS != "linux" || os.Geteuid() != 0 {
		return nil, errUnsupported
	}
	return readQualificationClient(ctx, name, 0, observeQualificationHost)
}

func readQualificationClient(ctx context.Context, name string, owner uint32, observe func(context.Context) (QualificationHost, error)) (*QualificationClient, error) {
	body, err := hostfs.ReadOwned(ctx, name, owner, 0o600, 1<<20)
	if err != nil {
		return nil, err
	}
	var cfg QualificationClientConfig
	if observe == nil || decodeCanonical(body, &cfg) != nil || !validQualificationConfig(name, cfg) {
		return nil, broker.ErrDenied
	}
	transport, err := qualificationTransport(ctx, cfg, owner)
	if err != nil {
		return nil, err
	}
	expected := evidence.Digest(body)
	trust := func(operation context.Context) error {
		current, readErr := hostfs.ReadOwned(operation, name, owner, 0o600, 1<<20)
		if readErr != nil {
			return readErr
		}
		if evidence.Digest(current) != expected {
			return broker.ErrConflict
		}
		for _, pin := range []hostfs.Pin{cfg.Certificate, cfg.TLSKey, cfg.ServerCAs} {
			if pinErr := hostfs.VerifyPin(operation, pin, owner); pinErr != nil {
				return pinErr
			}
		}
		return operation.Err()
	}
	client := &QualificationClient{cfg: cfg, transport: transport, trust: trust, observe: observe, slots: make(chan struct{}, cfg.Readers)}
	client.http = &http.Client{Transport: transport, Timeout: cfg.Timeout, CheckRedirect: func(*http.Request, []*http.Request) error { return broker.ErrDenied }}
	if err = trust(ctx); err != nil {
		client.Close()
		return nil, err
	}
	return client, nil
}

func validQualificationConfig(name string, cfg QualificationClientConfig) bool {
	endpoint, err := url.Parse(cfg.Endpoint)
	if err != nil || endpoint.Scheme != "https" || endpoint.Hostname() == "" || endpoint.User != nil || endpoint.Path != "/v1/qualification" || endpoint.RawPath != "" || endpoint.RawQuery != "" || endpoint.ForceQuery || endpoint.Fragment != "" || endpoint.Opaque != "" || endpoint.String() != cfg.Endpoint {
		return false
	}
	if cfg.SchemaVersion != 1 || !validPolicyPrincipal(cfg.Source) || !evidence.ValidDigest(cfg.ServerSPKI) || cfg.Timeout < time.Second || cfg.Timeout > 10*time.Second || cfg.MaxEvidenceAge < time.Minute || cfg.MaxEvidenceAge > 24*time.Hour || cfg.Readers < 1 || cfg.Readers > 32 {
		return false
	}
	if !validDaemonPin(cfg.Certificate, daemonCertificatePEMBytes) || !validDaemonPin(cfg.TLSKey, daemonTLSKeyPEMBytes) || !validDaemonPin(cfg.ServerCAs, daemonClientCAPEMBytes) {
		return false
	}
	seen := map[string]bool{name: true}
	for _, pin := range []hostfs.Pin{cfg.Certificate, cfg.TLSKey, cfg.ServerCAs} {
		if !profilePath(pin.Path) || seen[pin.Path] {
			return false
		}
		seen[pin.Path] = true
	}
	return true
}

// Close releases idle authority connections. The owner must first join Verify.
func (client *QualificationClient) Close() { client.transport.CloseIdleConnections() }

// Verify returns capabilities only after an authenticated, fresh authority
// decision and a second local host/trust check. It never falls back to fixtures.
func (client *QualificationClient) Verify(ctx context.Context, deployment DeploymentIdentity) (DeploymentQualification, error) {
	if !validPolicyPrincipal(deployment.Broker) || !evidence.ValidDigest(deployment.ProfileDigest) || !evidence.ValidDigest(deployment.RuntimeDigest) {
		return DeploymentQualification{}, broker.ErrDenied
	}
	operation, cancel := context.WithTimeout(ctx, client.cfg.Timeout)
	defer cancel()
	select {
	case client.slots <- struct{}{}:
		defer func() { <-client.slots }()
	default:
		return DeploymentQualification{}, broker.ErrDenied
	}
	if err := client.trust(operation); err != nil {
		return DeploymentQualification{}, err
	}
	host, err := client.observe(operation)
	if err != nil || !validQualificationHost(host) {
		return DeploymentQualification{}, errors.Join(err, broker.ErrDenied)
	}
	nonce := make([]byte, 32)
	if _, err = rand.Read(nonce); err != nil {
		return DeploymentQualification{}, broker.ErrDenied
	}
	request := QualificationRequest{SchemaVersion: 1, Source: client.cfg.Source, Nonce: hex.EncodeToString(nonce), Host: host, Deployment: deployment}
	started := time.Now()
	response, err := client.request(operation, request)
	if err != nil || !client.validResponse(request, response, started, time.Now()) {
		return DeploymentQualification{}, errors.Join(err, broker.ErrDenied)
	}
	if trustErr := client.trust(operation); trustErr != nil {
		return DeploymentQualification{}, trustErr
	}
	current, err := client.observe(operation)
	if err != nil || current != host || operation.Err() != nil {
		return DeploymentQualification{}, errors.Join(err, operation.Err(), broker.ErrDenied)
	}
	// Recheck expiry after local verification, which can consume the remaining lease.
	if validationErr := response.Qualification.Capabilities.Validate(); validationErr != nil {
		return DeploymentQualification{}, validationErr
	}
	return response.Qualification, nil
}

func (client *QualificationClient) request(ctx context.Context, value QualificationRequest) (QualificationResponse, error) {
	body, err := json.Marshal(value)
	if err != nil {
		return QualificationResponse{}, broker.ErrDenied
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, client.cfg.Endpoint, bytes.NewReader(body))
	if err != nil {
		return QualificationResponse{}, broker.ErrDenied
	}
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Accept", "application/json")
	request.Header.Set("Cache-Control", "no-store")
	response, err := client.http.Do(request)
	if err != nil {
		return QualificationResponse{}, broker.ErrDenied
	}
	defer func() { _ = response.Body.Close() }()
	if response.StatusCode != http.StatusOK || response.Header.Get("Content-Type") != "application/json" || response.Header.Get("Content-Encoding") != "" || response.ContentLength > qualificationResponseBytes {
		return QualificationResponse{}, broker.ErrDenied
	}
	payload, err := io.ReadAll(io.LimitReader(response.Body, qualificationResponseBytes+1))
	if err != nil || len(payload) > qualificationResponseBytes {
		return QualificationResponse{}, broker.ErrDenied
	}
	var result QualificationResponse
	if decodeCanonical(payload, &result) != nil {
		return QualificationResponse{}, broker.ErrDenied
	}
	return result, ctx.Err()
}

func (client *QualificationClient) validResponse(request QualificationRequest, response QualificationResponse, started, finished time.Time) bool {
	if response.SchemaVersion != 1 || response.Source != request.Source || response.Nonce != request.Nonce || response.Host != request.Host || response.Status != "qualified" || response.Qualification.Deployment != request.Deployment || response.Qualification.Capabilities.RuntimeDigest != request.Deployment.RuntimeDigest || response.Qualification.Capabilities.Validate() != nil {
		return false
	}
	// A five-second clock tolerance applies only to the online check, never expiry.
	if response.CheckedAt.Before(started.Add(-5*time.Second)) || response.CheckedAt.After(finished.Add(5*time.Second)) || !response.ValidUntil.After(finished) || response.Qualification.Capabilities.QualifiedUntil.After(response.ValidUntil) {
		return false
	}
	record := response.Evidence
	if record.Kind != "live-systemd-runsc-v1" || !validPolicyPrincipal(record.Reference) || !evidence.ValidDigest(record.SHA256) || record.ControlsDigest != request.Deployment.ProfileDigest || !slices.Equal(record.Checks, qualificationChecks()) {
		return false
	}
	if record.ObservedAt.IsZero() || record.ObservedAt.After(response.CheckedAt) || record.ObservedAt.After(finished) || finished.Sub(record.ObservedAt) > client.cfg.MaxEvidenceAge || response.ValidUntil.After(record.ObservedAt.Add(client.cfg.MaxEvidenceAge)) {
		return false
	}
	return !strings.HasPrefix(response.Qualification.Capabilities.Qualification, "fixture")
}

func validQualificationHost(host QualificationHost) bool {
	if !evidence.ValidDigest(host.MachineID) || len(host.BootID) != 36 {
		return false
	}
	for index, char := range host.BootID {
		if index == 8 || index == 13 || index == 18 || index == 23 {
			if char != '-' {
				return false
			}
			continue
		}
		if !strings.ContainsRune("0123456789abcdef", char) {
			return false
		}
	}
	return validPolicyPrincipal(host.KernelRelease) && validPolicyPrincipal(host.KernelVersion) && validPolicyPrincipal(host.Architecture)
}

func qualificationChecks() []string {
	return []string{"artifact-export", "broker-worker-restart", "cpu-memory-process-limits", "credential-isolation", "deployed-image-pins", "descendant-cleanup", "effective-service-controls", "host-file-denial", "network-dns-redirect-denial", "protected-grading", "read-only-root-inputs", "scratch-limit", "wall-output-supervision"}
}
