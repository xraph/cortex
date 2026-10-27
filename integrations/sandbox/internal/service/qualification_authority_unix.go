//go:build linux || darwin

package service

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/xraph/cortex/evidence"
	broker "github.com/xraph/cortex/integrations/sandbox"
	"github.com/xraph/cortex/integrations/sandbox/internal/hostfs"
)

const qualificationRecordDomain = "cortex-sandbox-qualification-record-v1\n"
const qualificationProbeBytes = 256 << 10

// QualificationAuthorityConfig bounds one authority lifetime. Policy is reread
// on every request; changing the other settings requires restarting the service.
type QualificationAuthorityConfig struct {
	SchemaVersion  int           `json:"schema_version"`
	Source         string        `json:"source"`
	Policy         string        `json:"policy"`
	MaxEvidenceAge time.Duration `json:"max_evidence_age_ns"`
	DecisionTTL    time.Duration `json:"decision_ttl_ns"`
	Timeout        time.Duration `json:"timeout_ns"`
	Readers        int           `json:"readers"`
}

// QualificationAuthorityPolicy is protected current authorization. Removing a
// grant or producer key revokes it. ValidUntil bounds even an unchanged policy.
type QualificationAuthorityPolicy struct {
	SchemaVersion int                           `json:"schema_version"`
	Source        string                        `json:"source"`
	ValidUntil    time.Time                     `json:"valid_until"`
	Grants        []QualificationAuthorityGrant `json:"grants"`
}

// QualificationAuthorityGrant authorizes one client certificate digest for an
// exact host, deployment and retained record signed by an approved producer.
type QualificationAuthorityGrant struct {
	ClientSHA256 string             `json:"client_sha256"`
	Host         QualificationHost  `json:"host"`
	Deployment   DeploymentIdentity `json:"deployment"`
	ProducerKey  []byte             `json:"producer_key"`
	Record       hostfs.Pin         `json:"record"`
}

// QualificationProbe binds a required check to retained go test -json output.
// The trusted producer must execute the named live test against this deployment.
// stdout text that merely says PASS is not an executable test result.
type QualificationProbe struct {
	Name    string     `json:"name"`
	Package string     `json:"package"`
	Test    string     `json:"test"`
	Output  hostfs.Pin `json:"output"`
}

// QualificationRecord is signed by an independent approved probe producer.
// Signature covers qualificationRecordDomain plus canonical JSON with a nil
// Signature. Paths and hashes bind retained output; no request can create it.
type QualificationRecord struct {
	SchemaVersion int                     `json:"schema_version"`
	Kind          string                  `json:"kind"`
	Source        string                  `json:"source"`
	Reference     string                  `json:"reference"`
	Host          QualificationHost       `json:"host"`
	Qualification DeploymentQualification `json:"qualification"`
	ObservedAt    time.Time               `json:"observed_at"`
	Probes        []QualificationProbe    `json:"probes"`
	Signature     []byte                  `json:"signature"`
}

// QualificationAuthority verifies retained evidence on every challenge. It has
// no success cache, probe execution route, or way to mint a producer signature.
type QualificationAuthority struct {
	cfg   QualificationAuthorityConfig
	owner uint32
	trust func(context.Context) error
	slots chan struct{}
}

func newQualificationAuthority(cfg QualificationAuthorityConfig, owner uint32, trust func(context.Context) error) (*QualificationAuthority, error) {
	if cfg.SchemaVersion != 1 || !validPolicyPrincipal(cfg.Source) || !profilePath(cfg.Policy) || cfg.MaxEvidenceAge < time.Minute || cfg.MaxEvidenceAge > 24*time.Hour || cfg.DecisionTTL < time.Second || cfg.DecisionTTL > time.Minute || cfg.Timeout < time.Second || cfg.Timeout > 10*time.Second || cfg.Readers < 1 || cfg.Readers > 32 || trust == nil {
		return nil, broker.ErrDenied
	}
	return &QualificationAuthority{cfg: cfg, owner: owner, trust: trust, slots: make(chan struct{}, cfg.Readers)}, nil
}

// ServeHTTP accepts only canonical challenges from verified TLS client
// certificates. Errors disclose neither retained paths nor evidence contents.
func (a *QualificationAuthority) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	if r.Method != http.MethodPost || r.URL.Path != "/v1/qualification" || r.URL.RawPath != "" || r.URL.RawQuery != "" || r.URL.ForceQuery || r.Header.Get("Content-Type") != "application/json" || r.Header.Get("Content-Encoding") != "" || r.TLS == nil || len(r.TLS.VerifiedChains) == 0 || len(r.TLS.PeerCertificates) == 0 {
		http.Error(w, "Forbidden", http.StatusForbidden)
		return
	}
	select {
	case a.slots <- struct{}{}:
		defer func() { <-a.slots }()
	default:
		http.Error(w, "Service Unavailable", http.StatusServiceUnavailable)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), a.cfg.Timeout)
	defer cancel()
	body, err := io.ReadAll(io.LimitReader(r.Body, qualificationResponseBytes+1))
	var request QualificationRequest
	if err != nil || len(body) > qualificationResponseBytes || decodeCanonical(body, &request) != nil {
		http.Error(w, "Forbidden", http.StatusForbidden)
		return
	}
	response, err := a.evaluate(ctx, request, evidence.Digest(r.TLS.PeerCertificates[0].Raw))
	if err != nil {
		http.Error(w, "Forbidden", http.StatusForbidden)
		return
	}
	payload, err := json.Marshal(response)
	if err != nil || len(payload) > qualificationResponseBytes || ctx.Err() != nil {
		http.Error(w, "Forbidden", http.StatusForbidden)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	if _, err = w.Write(payload); err != nil {
		return
	} // The transport owns connection cleanup.
}

func validQualificationDeployment(d DeploymentIdentity) bool {
	return validPolicyPrincipal(d.Broker) && evidence.ValidDigest(d.ProfileDigest) && evidence.ValidDigest(d.RuntimeDigest)
}
func (a *QualificationAuthority) policy(ctx context.Context) (QualificationAuthorityPolicy, []byte, error) {
	body, err := hostfs.ReadOwned(ctx, a.cfg.Policy, a.owner, 0o600, 1<<20)
	var p QualificationAuthorityPolicy
	if err != nil || decodeCanonical(body, &p) != nil || p.SchemaVersion != 1 || p.Source != a.cfg.Source || !p.ValidUntil.After(time.Now()) || len(p.Grants) > 64 {
		return p, nil, broker.ErrDenied
	}
	seen := make(map[string]bool)
	for _, g := range p.Grants {
		keyBytes, marshalErr := json.Marshal(struct {
			Client     string
			Host       QualificationHost
			Deployment DeploymentIdentity
		}{g.ClientSHA256, g.Host, g.Deployment})
		key := string(keyBytes)
		if marshalErr != nil || seen[key] || !evidence.ValidDigest(g.ClientSHA256) || !validQualificationHost(g.Host) || !validQualificationDeployment(g.Deployment) || len(g.ProducerKey) != ed25519.PublicKeySize || !validDaemonPin(g.Record, qualificationResponseBytes) || !profilePath(g.Record.Path) || g.Record.Path == a.cfg.Policy {
			return p, nil, broker.ErrDenied
		}
		seen[key] = true
	}
	return p, body, nil
}
func (a *QualificationAuthority) evaluate(ctx context.Context, request QualificationRequest, client string) (QualificationResponse, error) {
	if ctx.Err() != nil || request.SchemaVersion != 1 || request.Source != a.cfg.Source || !evidence.ValidDigest(request.Nonce) || !evidence.ValidDigest(client) || !validQualificationHost(request.Host) || !validQualificationDeployment(request.Deployment) {
		return QualificationResponse{}, broker.ErrDenied
	}
	p, policyBytes, err := a.policy(ctx)
	if err != nil {
		return QualificationResponse{}, err
	}
	var grant *QualificationAuthorityGrant
	for i := range p.Grants {
		g := &p.Grants[i]
		if g.ClientSHA256 == client && g.Host == request.Host && g.Deployment == request.Deployment {
			grant = g
			break
		}
	}
	if grant == nil {
		return QualificationResponse{}, broker.ErrDenied
	}
	body, err := hostfs.ReadPin(ctx, grant.Record, a.owner)
	var record QualificationRecord
	if err != nil || decodeCanonical(body, &record) != nil {
		return QualificationResponse{}, broker.ErrDenied
	}
	signature := record.Signature
	record.Signature = nil
	signed, err := json.Marshal(record)
	if err != nil || !ed25519.Verify(grant.ProducerKey, append([]byte(qualificationRecordDomain), signed...), signature) {
		return QualificationResponse{}, broker.ErrDenied
	}
	now := time.Now().UTC()
	if record.SchemaVersion != 1 || record.Kind != "live-systemd-runsc-v1" || record.Source != request.Source || !validPolicyPrincipal(record.Reference) || record.Host != request.Host || record.Qualification.Deployment != request.Deployment || record.Qualification.Capabilities.RuntimeDigest != request.Deployment.RuntimeDigest || record.Qualification.Capabilities.Validate() != nil || strings.HasPrefix(record.Qualification.Capabilities.Qualification, "fixture") || record.ObservedAt.IsZero() || record.ObservedAt.After(now) || now.Sub(record.ObservedAt) > a.cfg.MaxEvidenceAge {
		return QualificationResponse{}, broker.ErrDenied
	}
	checks := qualificationChecks()
	if len(record.Probes) != len(checks) {
		return QualificationResponse{}, broker.ErrDenied
	}
	earliest := record.ObservedAt
	for i, probe := range record.Probes {
		if probe.Name != checks[i] || !validPolicyPrincipal(probe.Package) || !validPolicyPrincipal(probe.Test) || !strings.HasPrefix(probe.Test, "TestLive") || !profilePath(probe.Output.Path) || !validDaemonPin(probe.Output, qualificationProbeBytes) {
			return QualificationResponse{}, broker.ErrDenied
		}
		output, readErr := hostfs.ReadPin(ctx, probe.Output, a.owner)
		first, passed := passedQualificationProbe(output, probe, record.ObservedAt, a.cfg.MaxEvidenceAge)
		if first.Before(earliest) {
			earliest = first
		}
		if readErr != nil || !passed {
			return QualificationResponse{}, broker.ErrDenied
		}
	}
	// Check configuration and credentials again after evidence I/O, then recheck
	// every retained byte and current policy before producing this decision.
	if err = a.trust(ctx); err != nil {
		return QualificationResponse{}, broker.ErrDenied
	}
	if err = hostfs.VerifyPin(ctx, grant.Record, a.owner); err != nil {
		return QualificationResponse{}, broker.ErrDenied
	}
	for _, probe := range record.Probes {
		if err = hostfs.VerifyPin(ctx, probe.Output, a.owner); err != nil {
			return QualificationResponse{}, broker.ErrDenied
		}
	}
	_, current, err := a.policy(ctx)
	if err != nil || !bytes.Equal(policyBytes, current) || ctx.Err() != nil {
		return QualificationResponse{}, broker.ErrDenied
	}
	now = time.Now().UTC()
	until := now.Add(a.cfg.DecisionTTL)
	for _, limit := range []time.Time{p.ValidUntil, earliest.Add(a.cfg.MaxEvidenceAge), record.Qualification.Capabilities.QualifiedUntil} {
		if limit.Before(until) {
			until = limit
		}
	}
	if !until.After(now) {
		return QualificationResponse{}, broker.ErrDenied
	}
	qualification := record.Qualification
	qualification.Capabilities.QualifiedUntil = until
	return QualificationResponse{SchemaVersion: 1, Source: request.Source, Nonce: request.Nonce, Host: request.Host, Status: "qualified", CheckedAt: now, ValidUntil: until, Qualification: qualification, Evidence: QualificationEvidence{Kind: record.Kind, Reference: record.Reference, SHA256: grant.Record.SHA256, ObservedAt: record.ObservedAt, ControlsDigest: request.Deployment.ProfileDigest, Checks: checks}}, nil
}
