package a2aremote_test

import (
	"context"
	"errors"
	"testing"

	"github.com/xraph/cortex"
	"github.com/xraph/cortex/a2aremote"
	"github.com/xraph/cortex/engine"
	"github.com/xraph/cortex/event"
	"github.com/xraph/cortex/id"
	"github.com/xraph/cortex/run"
)

type replayAuthorizer func(context.Context, id.AgentRunID) error

func (a replayAuthorizer) AuthorizePublicReplay(ctx context.Context, runID id.AgentRunID) error {
	return a(ctx, runID)
}

func TestRemoteReplayEngineAuthorizationAndScope(t *testing.T) {
	scope := cortex.Scope{Levels: []cortex.Level{{Key: "tenant", Value: "acme"}}}
	ctx := cortex.WithScope(t.Context(), scope)
	_, storage := newEngine(ctx, t, "replay", &scriptedLLM{}, "worker")
	owner, err := storage.GetByName(ctx, "worker")
	if err != nil {
		t.Fatal(err)
	}
	r := &run.Run{ID: id.NewAgentRunID(), AgentID: owner.ID, State: run.StateCompleted, Output: "SECRET candidate"}
	if err = storage.CreateRun(ctx, r); err != nil {
		t.Fatal(err)
	}
	if _, err = storage.AppendPublicEvents(ctx, event.AppendRequest{SchemaVersion: 1, RunID: r.ID, ExpectedRunRevision: r.Revision, Events: []event.Input{{ID: id.New(event.PrefixEvent), Type: event.ExecutionFinished}}}); err != nil {
		t.Fatal(err)
	}
	calls, denyAt := 0, 0
	eng, err := engine.New(engine.WithStore(storage), engine.WithPublicEventReplay(replayAuthorizer(func(ctx context.Context, runID id.AgentRunID) error {
		calls++
		peer, ok := a2aremote.PeerFromContext(ctx)
		if !ok || peer.Node != "trusted" || peer.Scope.Canonical() != scope.Canonical() || runID != r.ID || calls == denyAt {
			return errors.New("SECRET denied")
		}
		return nil
	})))
	if err != nil {
		t.Fatal(err)
	}
	resolverScope := scope
	resolver := a2aremote.ResolverFunc(func(context.Context, a2aremote.Credentials) (a2aremote.Peer, error) {
		return a2aremote.Peer{Node: "trusted", Scope: resolverScope}, nil
	})
	svc := a2aremote.NewService(a2aremote.EngineGateway(eng), resolver, a2aremote.Options{PublicEventReplay: true})
	if svc == nil {
		t.Fatal("configured engine unavailable")
	}
	credentials := a2aremote.Credentials{Headers: map[string][]string{"A2A-Extensions": {a2aremote.ReplayExtensionURI}}}
	req := a2aremote.GetTaskRequest{Tenant: "worker", ID: r.ID.String()}
	task, err := svc.GetTask(t.Context(), credentials, req)
	if err != nil || !a2aremote.ReplayActive(task) || calls != 2 {
		t.Fatalf("engine replay: %v %v, grants=%d", task, err, calls)
	}
	for _, deny := range []int{1, 2} {
		calls, denyAt = 0, deny
		if task, err = svc.GetTask(t.Context(), credentials, req); err == nil || task != nil {
			t.Fatalf("grant %d bypassed", deny)
		}
	}
	calls, denyAt = 0, 0
	resolverScope = cortex.Scope{Levels: []cortex.Level{{Key: "tenant", Value: "other"}}}
	if task, err = svc.GetTask(ctx, credentials, req); err == nil || task != nil || calls != 0 {
		t.Fatal("caller context escaped resolved scope")
	}
}

func TestRemoteSubscriptionUsesCommittedEngineLog(t *testing.T) {
	for _, revoke := range []bool{false, true} {
		scope := cortex.Scope{Levels: []cortex.Level{{Key: "tenant", Value: "follow"}}}
		ctx := cortex.WithScope(t.Context(), scope)
		_, storage := newEngine(ctx, t, "follow", &scriptedLLM{}, "worker")
		owner, err := storage.GetByName(ctx, "worker")
		if err != nil {
			t.Fatal(err)
		}
		current := &run.Run{ID: id.NewAgentRunID(), AgentID: owner.ID, State: run.StateRunning}
		if err = storage.CreateRun(ctx, current); err != nil {
			t.Fatal(err)
		}
		allowed := true
		eng, err := engine.New(engine.WithStore(storage), engine.WithPublicEventRecording(), engine.WithPublicEventReplay(replayAuthorizer(func(context.Context, id.AgentRunID) error {
			if !allowed {
				return errors.New("SECRET revoked")
			}
			return nil
		})))
		if err != nil {
			t.Fatal(err)
		}
		svc := a2aremote.NewService(a2aremote.EngineGateway(eng), a2aremote.ResolverFunc(func(context.Context, a2aremote.Credentials) (a2aremote.Peer, error) {
			return a2aremote.Peer{Node: "trusted", Scope: scope}, nil
		}), a2aremote.Options{Streaming: true, PublicEventReplay: true})
		credentials := a2aremote.Credentials{Headers: map[string][]string{"A2A-Extensions": {a2aremote.ReplayExtensionURI}}}
		var opening string
		sent := 0
		err = svc.SubscribeTask(t.Context(), credentials, "worker", current.ID.String(), func(ev a2aremote.StreamEvent) error {
			sent++
			if ev.Task != nil {
				opening = ev.Task.Metadata[a2aremote.ReplayMetadataKey].(a2aremote.ReplayPageMetadata).NextCursor
				if revoke {
					allowed = false
					return nil
				}
				current.State = run.StateCompleted
				current.Output = "SECRET answer"
				return eng.UpdateRun(ctx, current)
			}
			if ev.StatusUpdate == nil || !ev.StatusUpdate.Final {
				t.Fatalf("bad completion: %+v", ev)
			}
			page := ev.StatusUpdate.Metadata[a2aremote.ReplayMetadataKey].(a2aremote.ReplayPageMetadata)
			if len(page.Events) != 1 || page.Events[0].Type != event.ExecutionFinished || page.Events[0].Sequence != "1" || ev.StatusUpdate.Status.Message != nil {
				t.Fatalf("bad durable page: %+v", page)
			}
			return nil
		})
		if revoke {
			if err == nil || sent != 1 {
				t.Fatalf("revocation bypass: %v sent=%d", err, sent)
			}
			continue
		}
		if err != nil || sent != 2 {
			t.Fatalf("follow failed: %v sent=%d", err, sent)
		}
		// A client that lost the final frame resumes its processed cursor with GetTask.
		credentials.Headers[a2aremote.ReplayCursorHeader] = []string{opening}
		task, err := svc.GetTask(t.Context(), credentials, a2aremote.GetTaskRequest{Tenant: "worker", ID: current.ID.String()})
		if err != nil {
			t.Fatal(err)
		}
		page := task.Metadata[a2aremote.ReplayMetadataKey].(a2aremote.ReplayPageMetadata)
		if len(page.Events) != 1 || len(task.Artifacts) != 0 {
			t.Fatalf("resume lost committed event: %+v", task)
		}
	}
}
