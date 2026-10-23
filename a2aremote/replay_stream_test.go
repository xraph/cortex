package a2aremote

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"testing/synctest"
	"time"

	"github.com/xraph/cortex/event"
	"github.com/xraph/cortex/id"
	"github.com/xraph/cortex/run"
	"github.com/xraph/cortex/verification"
)

func TestReplaySubscriptionDrainsBeforeTerminal(t *testing.T) {
	svc, gw, runID := replayServiceFixture(t)
	svc.opts.Streaming = true
	current, err := gw.GetRun(t.Context(), runID)
	if err != nil {
		t.Fatal(err)
	}
	current.State = run.StateRunning
	gw.addRun(current)
	resultID := id.New(verification.PrefixResult)
	entries := []event.Event{}
	gw.read = func(_ context.Context, req event.ReadRequest) (event.Page, error) {
		last := uint64(len(entries))
		end := min(last, req.After.Sequence+uint64(req.Limit))
		return event.Page{Watermark: event.Watermark{SchemaVersion: 1, RunID: runID, FirstSequence: 1, LastSequence: last, RunRevision: 2}, Next: event.Cursor{SchemaVersion: 1, RunID: runID, Sequence: end}, Events: append([]event.Event(nil), entries[req.After.Sequence:end]...)}, nil
	}
	cred := replayCredentials()
	cred.Headers[ReplayLimitHeader] = []string{"1"}
	var frames []StreamEvent
	err = svc.SubscribeTask(t.Context(), cred, "worker", runID.String(), func(ev StreamEvent) error {
		frames = append(frames, ev)
		if ev.Task != nil {
			current.State = run.StateCompleted
			gw.addRun(current)
			for i, kind := range []event.Type{event.VerificationPassed, event.ArtifactCreated, event.ResultCommitted} {
				prefix := event.PrefixEvent
				if kind == event.ResultCommitted {
					prefix = verification.PrefixCommitEvent
				}
				entries = append(entries, event.Event{SchemaVersion: 1, Input: event.Input{ID: id.New(prefix), Type: kind, ResultID: resultID}, RunID: runID, Sequence: uint64(i + 1), RunRevision: 2, ObservedAt: time.Now().UTC()})
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(frames) != 4 || frames[0].Task == nil {
		t.Fatalf("bad frames: %+v", frames)
	}
	for i, ev := range frames[1:] {
		if ev.StatusUpdate == nil || ev.StatusUpdate.Final != (i == 2) || ev.StatusUpdate.Status.State.Terminal() != (i == 2) {
			t.Fatalf("premature terminal: %+v", ev)
		}
		page := ev.StatusUpdate.Metadata[ReplayMetadataKey].(ReplayPageMetadata)
		if len(page.Events) != 1 || page.Events[0].Sequence != []string{"1", "2", "3"}[i] {
			t.Fatalf("bad ordering: %+v", page)
		}
	}
	body, err := json.Marshal(frames)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(body), "SECRET") {
		t.Fatalf("private output: %s", body)
	}
}

func TestReplaySubscriptionPinsPeerAndStopsOnConsumerFailure(t *testing.T) {
	for _, mode := range []string{"node", "scope", "consumer"} {
		t.Run(mode, func(t *testing.T) {
			svc, gw, runID := replayServiceFixture(t)
			svc.opts.Streaming = true
			current, _ := gw.GetRun(t.Context(), runID)
			current.State = run.StateRunning
			gw.addRun(current)
			sent := 0
			stop := errors.New("consumer closed")
			err := svc.SubscribeTask(t.Context(), replayCredentials(), "worker", runID.String(), func(StreamEvent) error {
				sent++
				if mode == "consumer" {
					return stop
				}
				svc.resolver = ResolverFunc(func(context.Context, Credentials) (Peer, error) {
					peer := Peer{Node: "trusted", Scope: testScope()}
					if mode == "node" {
						peer.Node = "replacement"
					} else {
						peer.Scope.Levels[0].Value = "other"
					}
					return peer, nil
				})
				return nil
			})
			if err == nil || sent != 1 {
				t.Fatalf("subscription continued: sent=%d err=%v", sent, err)
			}
		})
	}
}

func TestReplaySubscriptionHasBoundedLifetime(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		svc, gw, runID := replayServiceFixture(t)
		svc.opts.Streaming = true
		current, _ := gw.GetRun(t.Context(), runID)
		current.State = run.StateRunning
		gw.addRun(current)
		start := time.Now()
		sends := 0
		err := svc.SubscribeTask(t.Context(), replayCredentials(), "worker", runID.String(), func(StreamEvent) error { sends++; return nil })
		if err == nil || sends < 2 || time.Since(start) != 5*time.Minute {
			t.Fatalf("unbounded follow: %v sends=%d elapsed=%v", err, sends, time.Since(start))
		}
	})
}

func TestReplaySubscriptionGapAndCapacity(t *testing.T) {
	svc, gw, runID := replayServiceFixture(t)
	svc.opts.Streaming = true
	current, _ := gw.GetRun(t.Context(), runID)
	current.State = run.StateRunning
	gw.addRun(current)
	reads := 0
	gw.read = func(_ context.Context, _ event.ReadRequest) (event.Page, error) {
		reads++
		return event.Page{Watermark: event.Watermark{SchemaVersion: 1, RunID: runID, FirstSequence: 3, LastSequence: 2, RunRevision: 2}, Next: event.Cursor{SchemaVersion: 1, RunID: runID, Sequence: 2}}, event.ErrCursorGap
	}
	svc.replayStreams.Store(maxReplaySubscriptions)
	if err := svc.SubscribeTask(t.Context(), replayCredentials(), "worker", runID.String(), func(StreamEvent) error { t.Fatal("capacity bypass"); return nil }); err == nil || reads != 0 {
		t.Fatalf("capacity bypass: %v", err)
	}
	svc.replayStreams.Store(0)
	sent := 0
	if err := svc.SubscribeTask(t.Context(), replayCredentials(), "worker", runID.String(), func(ev StreamEvent) error {
		sent++
		if ev.Task == nil || !ev.Task.Metadata[ReplayMetadataKey].(ReplayPageMetadata).Gap {
			t.Fatalf("missing recovery: %+v", ev)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if sent != 1 || reads != 1 || svc.replayStreams.Load() != 0 {
		t.Fatalf("gap kept following: sent=%d reads=%d", sent, reads)
	}
}

func TestReplaySubscriptionCardMethods(t *testing.T) {
	svc, gw, _ := replayServiceFixture(t)
	agent, err := gw.GetAgentByName(t.Context(), "worker")
	if err != nil {
		t.Fatal(err)
	}
	for _, streaming := range []bool{false, true} {
		card := BuildCard(agent, nil, CardOptions{PublicEventReplay: svc.opts.PublicEventReplay, Streaming: streaming})
		for _, extension := range card.Capabilities.Extensions {
			if extension.URI != ReplayExtensionURI {
				continue
			}
			methods := extension.Params["methods"].([]string)
			if len(methods) != 1 && !streaming || streaming && (len(methods) != 2 || methods[1] != MethodSubscribeToTask) {
				t.Fatalf("misleading methods: %v", methods)
			}
		}
	}
}

func TestReplaySubscriptionCancellationReleasesCapacity(t *testing.T) {
	svc, gw, runID := replayServiceFixture(t)
	svc.opts.Streaming = true
	current, _ := gw.GetRun(t.Context(), runID)
	current.State = run.StateRunning
	gw.addRun(current)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	entered := make(chan struct{}, maxReplaySubscriptions)
	completed := make(chan error, maxReplaySubscriptions)
	for range maxReplaySubscriptions {
		go func() {
			completed <- svc.SubscribeTask(ctx, replayCredentials(), "worker", runID.String(), func(StreamEvent) error {
				entered <- struct{}{}
				<-ctx.Done()
				return ctx.Err()
			})
		}()
	}
	for range maxReplaySubscriptions {
		select {
		case <-entered:
		case <-time.After(5 * time.Second):
			t.Fatal("subscription did not enter")
		}
	}
	if svc.replayStreams.Load() != maxReplaySubscriptions {
		t.Fatal("lost admitted stream")
	}
	if err := svc.SubscribeTask(t.Context(), replayCredentials(), "worker", runID.String(), func(StreamEvent) error { t.Fatal("admitted overflow"); return nil }); err == nil {
		t.Fatal("capacity exceeded")
	}
	cancel()
	for range maxReplaySubscriptions {
		select {
		case err := <-completed:
			if err == nil {
				t.Fatal("cancel was lost")
			}
		case <-time.After(5 * time.Second):
			t.Fatal("subscription survived cancellation")
		}
	}
	if svc.replayStreams.Load() != 0 {
		t.Fatal("capacity not released")
	}
}
