// Package grading connects protected verification to authenticated sandbox jobs.
package grading

import (
	"context"
	"crypto/ed25519"
	"strings"
	"unicode/utf8"

	broker "github.com/xraph/cortex/integrations/sandbox"
	"github.com/xraph/cortex/verification"

	"github.com/xraph/cortex/sandbox"
)

// Authenticity pins the broker name and signing key independently of responses.
type Authenticity struct {
	name string
	key  ed25519.PublicKey
}

// NewAuthenticity copies a host-configured broker identity.
func NewAuthenticity(name string, key ed25519.PublicKey) (*Authenticity, error) {
	if name == "" || strings.TrimSpace(name) != name || len(name) > 512 || !utf8.ValidString(name) || strings.ContainsRune(name, 0) || len(key) != ed25519.PublicKeySize {
		return nil, broker.ErrDenied
	}
	return &Authenticity{name: name, key: append(ed25519.PublicKey(nil), key...)}, nil
}

// VerifyReceipt authenticates the complete receipt against the expected job.
func (a *Authenticity) VerifyReceipt(ctx context.Context, r sandbox.Receipt, j sandbox.Job) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if a == nil || r.Broker != a.name || j.Validate() != nil || r.Job.Digest != j.Digest {
		return broker.ErrDenied
	}
	if err := broker.VerifyReceipt(r, a.key); err != nil {
		return err
	}
	return ctx.Err()
}

// VerifyResult authenticates a terminal observation without treating it as a verdict.
func (a *Authenticity) VerifyResult(ctx context.Context, r sandbox.Result, j sandbox.Job) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if a == nil {
		return broker.ErrDenied
	}
	if err := broker.VerifyResult(r, j, a.key); err != nil {
		return err
	}
	return ctx.Err()
}

var _ verification.SandboxAuthenticity = (*Authenticity)(nil)
