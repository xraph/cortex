package a2a

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/xraph/cortex"
	"github.com/xraph/cortex/id"
)

type recipientDiscoveryStub struct {
	Store
	list func(context.Context, id.DeliveryID, int) ([]id.DeliveryID, error)
}

func (s *recipientDiscoveryStub) ListDeferredRecipients(ctx context.Context, after id.DeliveryID, limit int) ([]id.DeliveryID, error) {
	return s.list(ctx, after, limit)
}

type recipientObserverStub func(context.Context, RecipientRequest) (RecipientObservation, error)

func (f recipientObserverStub) ObserveRecipient(ctx context.Context, req RecipientRequest) (RecipientObservation, error) {
	return f(ctx, req)
}

func recipientDiscoveryFixture(t *testing.T) (*Bus, *recipientDiscoveryStub, context.Context) {
	t.Helper()
	ctx := testCtx()
	team := id.NewOrchestrationID()
	roster, err := freezeRoster(&Roster{Scope: cortex.ScopeFromContext(ctx), Participants: []RosterParticipant{{Name: "worker", AgentID: id.NewAgentID()}}}, team)
	require.NoError(t, err)
	backend := &recipientDiscoveryStub{list: func(context.Context, id.DeliveryID, int) ([]id.DeliveryID, error) { return nil, nil }}
	b := &Bus{store: backend, roster: roster, orchestrationID: team, reserved: unavailableReservation{}, recipientObserver: recipientObserverStub(func(context.Context, RecipientRequest) (RecipientObservation, error) {
		t.Fatal("unexpected child observation")
		return RecipientObservation{}, nil
	})}
	require.NoError(t, b.configureRecipientRecovery(&RecipientRecoveryConfig{BatchSize: 2, AuthorizeDiscovery: func(context.Context, id.OrchestrationID) error { return nil }, AuthorizeDelivery: func(context.Context, id.OrchestrationID, id.DeliveryID) error { return nil }}, backend))
	return b, backend, ctx
}

func TestRecipientDiscoveryRejectsMalformedPagesBeforePrivateReads(t *testing.T) {
	first, second := id.NewDeliveryID(), id.NewDeliveryID()
	for _, tc := range []struct {
		name  string
		rows  []id.DeliveryID
		after id.DeliveryID
	}{
		{"duplicate", []id.DeliveryID{first, first}, id.Nil},
		{"descending", []id.DeliveryID{second, first}, id.Nil},
		{"oversized", []id.DeliveryID{first, second, id.NewDeliveryID()}, id.Nil},
		{"wrong_kind", []id.DeliveryID{id.NewAgentRunID()}, id.Nil},
		{"inclusive", []id.DeliveryID{first}, first},
		{"zero", []id.DeliveryID{id.Nil}, id.Nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			b, backend, ctx := recipientDiscoveryFixture(t)
			backend.list = func(context.Context, id.DeliveryID, int) ([]id.DeliveryID, error) { return tc.rows, nil }
			page, err := b.RecoverRecipients(ctx, tc.after)
			require.ErrorIs(t, err, ErrReplyCommitConflict)
			require.Zero(t, page)
		})
	}
}

func TestRecipientDiscoveryAuthorizationAndTimeout(t *testing.T) {
	for _, mode := range []string{"denied", "revoked", "cancelled", "timeout", "store_error"} {
		t.Run(mode, func(t *testing.T) {
			b, backend, ctx := recipientDiscoveryFixture(t)
			denied := errors.New("denied")
			checks, reads := 0, 0
			b.recipientRecovery.AttemptTimeout = time.Millisecond
			b.recipientRecovery.AuthorizeDiscovery = func(context.Context, id.OrchestrationID) error {
				checks++
				if mode == "denied" || (mode == "revoked" && checks > 1) {
					return denied
				}
				return nil
			}
			backend.list = func(ctx context.Context, _ id.DeliveryID, _ int) ([]id.DeliveryID, error) {
				reads++
				if mode == "timeout" {
					<-ctx.Done()
					return nil, ctx.Err()
				}
				if mode == "store_error" {
					return nil, denied
				}
				return nil, nil
			}
			if mode == "cancelled" {
				cancelled, cancel := context.WithCancel(ctx)
				cancel()
				ctx = cancelled
			}
			page, err := b.RecoverRecipients(ctx, id.Nil)
			require.Error(t, err)
			require.Zero(t, page)
			if mode == "denied" || mode == "cancelled" {
				require.Zero(t, reads)
			}
		})
	}
}

func TestRecipientRecoveryRequiresQualifiedConfiguration(t *testing.T) {
	for _, mode := range []string{"no_roster", "no_runner", "no_observer", "typed_nil_observer", "no_discovery_authority", "no_delivery_authority", "negative_batch", "large_batch", "negative_timeout", "large_timeout", "unsupported_store"} {
		t.Run(mode, func(t *testing.T) {
			b, backend, _ := recipientDiscoveryFixture(t)
			cfg := *b.recipientRecovery
			var source Store = backend
			switch mode {
			case "no_roster":
				b.roster = nil
			case "no_runner":
				b.reserved = nil
			case "no_observer":
				b.recipientObserver = nil
			case "typed_nil_observer":
				b.recipientObserver = recipientObserverStub(nil)
			case "no_discovery_authority":
				cfg.AuthorizeDiscovery = nil
			case "no_delivery_authority":
				cfg.AuthorizeDelivery = nil
			case "negative_batch":
				cfg.BatchSize = -1
			case "large_batch":
				cfg.BatchSize = 257
			case "negative_timeout":
				cfg.AttemptTimeout = -1
			case "large_timeout":
				cfg.AttemptTimeout = 2 * time.Minute
			case "unsupported_store":
				source = newMemStore()
			}
			require.Error(t, b.configureRecipientRecovery(&cfg, source))
		})
	}
	b, backend, _ := recipientDiscoveryFixture(t)
	cfg := *b.recipientRecovery
	cfg.BatchSize, cfg.AttemptTimeout = 0, 0
	require.NoError(t, b.configureRecipientRecovery(&cfg, backend))
	cfg.BatchSize = 257
	require.Equal(t, 32, b.recipientRecovery.BatchSize)
	require.Equal(t, 5*time.Second, b.recipientRecovery.AttemptTimeout)
}
