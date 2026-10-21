package workerstore

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"
)

func TestVerifyClaimRejectsStaleRevokedAndExpiredCapabilities(t *testing.T) {
	cfg, receipt := fixture(t)
	now := receipt.AcceptedAt
	cfg.Now = func() time.Time { return now }
	store := openStore(t, cfg)
	if _, err := store.Enroll(t.Context(), receipt); err != nil {
		t.Fatal(err)
	}
	claim, err := store.Claim(t.Context(), receipt.Job.ID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = store.VerifyClaim(t.Context(), receipt.Job.ID, claim.Token, claim.Record.Revision); err != nil {
		t.Fatal(err)
	}
	for _, test := range []struct {
		token    string
		revision uint64
	}{{strings.Repeat("0", 64), claim.Record.Revision}, {claim.Token, claim.Record.Revision + 1}} {
		if _, err = store.VerifyClaim(t.Context(), receipt.Job.ID, test.token, test.revision); !errors.Is(err, ErrConflict) {
			t.Fatal("foreign or stale claim verified", err)
		}
	}
	now = receipt.AcceptedAt.Add(receipt.Job.Limits.WallTime)
	if _, err = store.VerifyClaim(t.Context(), receipt.Job.ID, claim.Token, claim.Record.Revision); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatal("expired claim verified", err)
	}
	now = receipt.AcceptedAt.Add(time.Second)
	if _, err = store.Cancel(t.Context(), receipt.Job.ID); err != nil {
		t.Fatal(err)
	}
	if _, err = store.VerifyClaim(t.Context(), receipt.Job.ID, claim.Token, claim.Record.Revision); !errors.Is(err, ErrConflict) {
		t.Fatal("revoked claim verified", err)
	}
}
