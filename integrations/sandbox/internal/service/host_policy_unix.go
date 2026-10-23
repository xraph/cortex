//go:build linux || darwin

package service

import (
	"bytes"
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/xraph/cortex"
	"github.com/xraph/cortex/evidence"
	broker "github.com/xraph/cortex/integrations/sandbox"
	"github.com/xraph/cortex/integrations/sandbox/internal/hostfs"
	"github.com/xraph/cortex/integrations/sandbox/internal/image"
	"github.com/xraph/cortex/sandbox"
)

// HostPolicyDocument is canonical JSON in a protected, replaceable host file.
// A document has at most 512 exact subjects and a finite validity interval.
// Empty Subjects revokes all access. Atomic replacement publishes an update.
type HostPolicyDocument struct {
	SchemaVersion int                `json:"schema_version"`
	NotBefore     time.Time          `json:"not_before"`
	NotAfter      time.Time          `json:"not_after"`
	Subjects      []HostSubjectGrant `json:"subjects"`
}

// HostSubjectGrant separately grants transport and bounded sandbox execution.
// Certificates contains up to 16 lowercase SHA256 leaf-certificate fingerprints.
// Empty Certificates revokes transport access while retaining the execution
// grant for recovery. Remove the subject to revoke execution as well. Images
// contains 1..256 immutable manifest pins. This grants no evidence access,
// artifact publication or cleanup authority and does not qualify the runtime.
type HostSubjectGrant struct {
	Principal    string         `json:"principal"`
	Scope        cortex.Scope   `json:"scope"`
	NotBefore    time.Time      `json:"not_before"`
	NotAfter     time.Time      `json:"not_after"`
	Certificates []string       `json:"certificates"`
	Images       []string       `json:"images"`
	Limits       sandbox.Limits `json:"limits"`
}

// HostPolicy rereads a bounded file on every check. Unreadable, invalid or
// expired policy denies access; no previous grant snapshot is retained.
type HostPolicy struct {
	path  string
	owner uint32
	now   func() time.Time
}

// NewHostPolicy requires a root-owned, single-link mode-0600 policy file.
// Keep its parent protected and publish updates by atomic replacement. Reads
// require responsive local storage, as kernel file calls cannot be interrupted
// forcibly by context cancellation. Construction validates the initial policy.
func NewHostPolicy(ctx context.Context, name string) (*HostPolicy, error) {
	return newHostPolicy(ctx, name, 0, time.Now)
}

func newHostPolicy(ctx context.Context, name string, owner uint32, now func() time.Time) (*HostPolicy, error) {
	p := &HostPolicy{path: name, owner: owner, now: now}
	if _, _, err := p.load(ctx); err != nil {
		return nil, err
	}
	return p, nil
}

func (p *HostPolicy) load(ctx context.Context) (HostPolicyDocument, time.Time, error) {
	var doc HostPolicyDocument
	if p == nil || p.now == nil {
		return doc, time.Time{}, broker.ErrDenied
	}
	body, err := hostfs.ReadOwned(ctx, p.path, p.owner, 0o600, 1<<20)
	if err != nil {
		return doc, time.Time{}, errors.Join(broker.ErrDenied, err)
	}
	now := p.now()
	if decodeCanonical(body, &doc) != nil || !validHostPolicy(doc) || !policyActive(doc.NotBefore, doc.NotAfter, now) {
		return HostPolicyDocument{}, now, broker.ErrDenied
	}
	return doc, now, ctx.Err()
}

func validHostPolicy(doc HostPolicyDocument) bool {
	if doc.SchemaVersion != 1 || doc.NotBefore.IsZero() || !doc.NotAfter.After(doc.NotBefore) || len(doc.Subjects) > 512 {
		return false
	}
	type subjectKey struct{ principal, scope string }
	subjects := make(map[subjectKey]bool, len(doc.Subjects))
	certificates := make(map[string]bool)
	for _, grant := range doc.Subjects {
		key := subjectKey{grant.Principal, grant.Scope.Canonical()}
		if strings.TrimSpace(grant.Principal) == "" || len(grant.Principal) > 512 || !utf8.ValidString(grant.Principal) || strings.ContainsRune(grant.Principal, 0) || cortex.ValidateScope(grant.Scope) != nil || subjects[key] || grant.NotBefore.IsZero() || !grant.NotAfter.After(grant.NotBefore) || grant.NotBefore.Before(doc.NotBefore) || grant.NotAfter.After(doc.NotAfter) || len(grant.Certificates) > 16 || len(grant.Images) == 0 || len(grant.Images) > 256 || !grant.Limits.Within(grant.Limits) {
			return false
		}
		subjects[key] = true
		for _, fingerprint := range grant.Certificates {
			if !evidence.ValidDigest(fingerprint) || certificates[fingerprint] {
				return false
			}
			certificates[fingerprint] = true
		}
		images := make(map[string]bool, len(grant.Images))
		for _, pin := range grant.Images {
			if !image.ValidDigest(pin) || images[pin] {
				return false
			}
			images[pin] = true
		}
	}
	return true
}

func policyActive(start, end, now time.Time) bool { return !now.Before(start) && now.Before(end) }

// Authenticate accepts only a completed TLS 1.3 connection with a verified,
// currently valid client chain and an explicit leaf fingerprint mapping. Subject
// names, request headers and payload fields cannot select another scope.
func (p *HostPolicy) Authenticate(request *http.Request) (broker.Identity, error) {
	if request == nil || request.TLS == nil || request.TLS.Version != tls.VersionTLS13 || !request.TLS.HandshakeComplete || len(request.TLS.PeerCertificates) == 0 || request.TLS.PeerCertificates[0] == nil {
		return broker.Identity{}, broker.ErrDenied
	}
	doc, now, err := p.load(request.Context())
	if err != nil {
		return broker.Identity{}, err
	}
	leaf := request.TLS.PeerCertificates[0]
	verified := false
	for _, chain := range request.TLS.VerifiedChains {
		if len(chain) == 0 || len(chain) > 16 || chain[0] == nil || !bytes.Equal(chain[0].Raw, leaf.Raw) {
			continue
		}
		current := true
		for _, certificate := range chain {
			if certificate == nil || len(certificate.Raw) == 0 || !policyActive(certificate.NotBefore, certificate.NotAfter, now) {
				current = false
				break
			}
		}
		if current {
			verified = true
			break
		}
	}
	if !verified {
		return broker.Identity{}, broker.ErrDenied
	}
	fingerprint := evidence.Digest(leaf.Raw)
	for _, grant := range doc.Subjects {
		if slices.Contains(grant.Certificates, fingerprint) && policyActive(grant.NotBefore, grant.NotAfter, now) {
			if finishErr := p.finish(request.Context(), doc, grant); finishErr != nil {
				return broker.Identity{}, finishErr
			}
			return broker.Identity{Principal: grant.Principal, Scope: cortex.Scope{Levels: slices.Clone(grant.Scope.Levels)}}, nil
		}
	}
	return broker.Identity{}, broker.ErrDenied
}

// Authorize binds a public operation's authenticated identity to its sealed job
// and current execution grant. Use it as the broker's Authorize callback.
func (p *HostPolicy) Authorize(ctx context.Context, who broker.Identity, job sandbox.Job) error {
	if cortex.ValidateScope(who.Scope) != nil || who.Principal != job.Principal || who.Scope.Canonical() != job.Scope.Canonical() {
		return broker.ErrDenied
	}
	return p.Revalidate(ctx, job)
}

// Revalidate checks the signed job subject without a live client connection.
// Use it for AuthorizeLaunch and Revalidate in HostConfig. The controller remains
// responsible for receipt verification, runtime qualification and worker fencing.
func (p *HostPolicy) Revalidate(ctx context.Context, job sandbox.Job) error {
	if err := job.Validate(); err != nil {
		return errors.Join(broker.ErrDenied, err)
	}
	doc, now, err := p.load(ctx)
	if err != nil {
		return err
	}
	for _, grant := range doc.Subjects {
		if grant.Principal == job.Principal && grant.Scope.Canonical() == job.Scope.Canonical() && policyActive(grant.NotBefore, grant.NotAfter, now) && slices.Contains(grant.Images, job.Image) && job.Limits.Within(grant.Limits) {
			return p.finish(ctx, doc, grant)
		}
	}
	return broker.ErrDenied
}

// AuthorizeImage checks the job's current manifest access. WorkerImages still
// validates membership and hashes for individual config and layer blobs.
func (p *HostPolicy) AuthorizeImage(ctx context.Context, job sandbox.Job, pin string) error {
	if pin != job.Image {
		return broker.ErrDenied
	}
	return p.Revalidate(ctx, job)
}

func (p *HostPolicy) finish(ctx context.Context, doc HostPolicyDocument, grant HostSubjectGrant) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	now := p.now()
	if !policyActive(doc.NotBefore, doc.NotAfter, now) || !policyActive(grant.NotBefore, grant.NotAfter, now) {
		return fmt.Errorf("%w: host grant expired", broker.ErrDenied)
	}
	return nil
}
