package a2aremote_test

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/xraph/cortex"
	"github.com/xraph/cortex/a2aremote"
	"github.com/xraph/cortex/engine"
	"github.com/xraph/cortex/internal/resulttest"
	"github.com/xraph/cortex/safety"
	"github.com/xraph/cortex/verification"
)

func TestRemoteContentEngineCurrentAuthority(t *testing.T) {
	scope := cortex.Scope{Levels: []cortex.Level{{Key: "tenant", Value: "content"}}}
	ctx := cortex.WithScope(t.Context(), scope)
	_, storage := newEngine(ctx, t, "content", &scriptedLLM{})
	receipt := resulttest.Commit(ctx, t, storage)
	allowed := true
	options := resulttest.Options(func(ctx context.Context, _ verification.ResultContentRequest) error {
		peer, ok := a2aremote.PeerFromContext(ctx)
		if !ok || peer.Node != "trusted" || peer.Scope.Canonical() != scope.Canonical() || !allowed {
			return verification.ErrResultAccessDenied
		}
		return nil
	}, func(context.Context, safety.PublicationRequest) (safety.PublicationDecision, error) {
		return safety.PublicationDecision{Text: "screened", Status: safety.StatusRedacted}, nil
	})
	eng, err := engine.New(append(options, engine.WithStore(storage))...)
	if err != nil {
		t.Fatal(err)
	}
	resolvedScope := scope
	svc := a2aremote.NewService(a2aremote.EngineGateway(eng), a2aremote.ResolverFunc(func(context.Context, a2aremote.Credentials) (a2aremote.Peer, error) {
		return a2aremote.Peer{Node: "trusted", Scope: resolvedScope}, nil
	}), a2aremote.Options{ResultContent: true})
	if svc == nil {
		t.Fatal("engine content gateway missing")
	}
	cred := a2aremote.Credentials{Headers: map[string][]string{"A2A-Extensions": {a2aremote.ResultContentExtensionURI}, a2aremote.ResultIDHeader: {receipt.Request.ResultID.String()}}}
	req := a2aremote.GetTaskRequest{Tenant: "result-reader", ID: receipt.Request.Current.RunID.String()}
	task, err := svc.GetTask(t.Context(), cred, req)
	if err != nil {
		t.Fatal(err)
	}
	body, _ := json.Marshal(task)
	if !a2aremote.ResultContentActive(task) || strings.Contains(string(body), "SECRET") || !strings.Contains(string(body), `"content":"screened"`) {
		t.Fatalf("unsafe engine projection: %s", body)
	}
	allowed = false
	if task, err = svc.GetTask(t.Context(), cred, req); err == nil || task != nil {
		t.Fatal("revoked content grant accepted")
	}
	allowed = true
	resolvedScope = cortex.Scope{Levels: []cortex.Level{{Key: "tenant", Value: "other"}}}
	if task, err = svc.GetTask(ctx, cred, req); err == nil || task != nil {
		t.Fatal("caller context escaped peer scope")
	}
	current, err := storage.GetRun(ctx, receipt.Request.Current.RunID)
	if err != nil || current.Output != "SECRET candidate" {
		t.Fatal("content read altered stored candidate")
	}
}

func TestRemoteContentRejectsAbsentEngine(t *testing.T) {
	resolver := a2aremote.ResolverFunc(func(context.Context, a2aremote.Credentials) (a2aremote.Peer, error) { return a2aremote.Peer{}, nil })
	if a2aremote.NewService(a2aremote.EngineGateway(nil), resolver, a2aremote.Options{ResultContent: true}) != nil {
		t.Fatal("absent engine accepted")
	}
}
