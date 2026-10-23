package a2aremote

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"testing/synctest"
	"time"

	"github.com/xraph/cortex"
	"github.com/xraph/cortex/a2a"
	"github.com/xraph/cortex/event"
	"github.com/xraph/cortex/id"
	"github.com/xraph/cortex/run"
)

type replayGateway struct {
	*fakeGateway
	read  func(context.Context, event.ReadRequest) (event.Page, error)
	ready bool
}

func (g *replayGateway) PublicEventReplayAvailable() bool { return g.ready }
func (g *replayGateway) ReadPublicEvents(ctx context.Context, r event.ReadRequest) (event.Page, error) {
	return g.read(ctx, r)
}
func replayCredentials() Credentials {
	return Credentials{Headers: map[string][]string{"A2A-Extensions": {ReplayExtensionURI}}}
}

func replayServiceFixture(t *testing.T) (*Service, *replayGateway, id.AgentRunID) {
	t.Helper()
	runID := id.NewAgentRunID()
	base := newFakeGateway()
	base.addRun(&run.Run{ID: runID, AgentID: base.agents["worker"].ID, State: run.StateCompleted, Revision: 2, Output: "SECRET candidate", Error: "SECRET provider"})
	gw := &replayGateway{fakeGateway: base, ready: true}
	gw.read = func(ctx context.Context, r event.ReadRequest) (event.Page, error) {
		peer, ok := PeerFromContext(ctx)
		if !ok || peer.Node == "" || peer.Scope.Canonical() != cortex.ScopeFromContext(ctx).Canonical() {
			t.Fatal("missing authenticated peer")
		}
		page := event.Page{Watermark: event.Watermark{SchemaVersion: 1, RunID: runID, FirstSequence: 1, LastSequence: 1, RunRevision: 2}, Next: event.Cursor{SchemaVersion: 1, RunID: runID, Sequence: 1}}
		if r.After.Sequence == 0 {
			page.Events = []event.Event{{SchemaVersion: 1, Input: event.Input{ID: id.New(event.PrefixEvent), Type: event.ExecutionFinished}, RunID: runID, Sequence: 1, RunRevision: 2, ObservedAt: time.Now().UTC()}}
		}
		return page, nil
	}
	svc := NewService(gw, okResolver(), Options{PublicEventReplay: true})
	if svc == nil {
		t.Fatal("replay service unavailable")
	}
	return svc, gw, runID
}

func TestRemoteReplayIsExplicitAndMetadataOnly(t *testing.T) {
	svc, _, runID := replayServiceFixture(t)
	plain, err := svc.GetTask(context.Background(), Credentials{}, GetTaskRequest{Tenant: "worker", ID: runID.String()})
	if err != nil || len(plain.Artifacts) != 1 || ReplayActive(plain) {
		t.Fatalf("legacy projection changed: %v %v", plain, err)
	}
	task, err := svc.GetTask(context.Background(), replayCredentials(), GetTaskRequest{Tenant: "worker", ID: runID.String()})
	if err != nil {
		t.Fatal(err)
	}
	body, err := json.Marshal(task)
	if err != nil {
		t.Fatal(err)
	}
	if !ReplayActive(task) || strings.Contains(string(body), "SECRET") || len(task.Artifacts) != 0 || task.Status.Message != nil {
		t.Fatalf("bad replay: %s", body)
	}
	var wire struct {
		Metadata map[string]ReplayPageMetadata `json:"metadata"`
	}
	if err = json.Unmarshal(body, &wire); err != nil {
		t.Fatal(err)
	}
	page := wire.Metadata[ReplayMetadataKey]
	if page.Gap || len(page.Events) != 1 || page.Events[0].Sequence != "1" || page.NextCursor == "" {
		t.Fatalf("bad page: %+v", page)
	}
	cred := replayCredentials()
	cred.Headers[ReplayCursorHeader] = []string{page.NextCursor}
	resumed, err := svc.GetTask(context.Background(), cred, GetTaskRequest{Tenant: "worker", ID: runID.String()})
	if err != nil {
		t.Fatal(err)
	}
	if len(resumed.Metadata[ReplayMetadataKey].(ReplayPageMetadata).Events) != 0 {
		t.Fatal("resume replayed consumed event")
	}
}

func TestRemoteReplayRejectsAmbiguousOrUnnegotiatedHeaders(t *testing.T) {
	for _, headers := range []map[string][]string{
		{ReplayCursorHeader: {"bad"}},
		{"A2A-Extensions": {ReplayExtensionURI}, ReplayLimitHeader: {"129"}},
		{"A2A-Extensions": {ReplayExtensionURI}, ReplayLimitHeader: {"01"}},
		{"A2A-Extensions": {ReplayExtensionURI}, ReplayCursorHeader: {"one", "two"}},
		{"A2A-Extensions": {ReplayExtensionURI}, ReplayCursorHeader: {""}, strings.ToLower(ReplayCursorHeader): {""}},
	} {
		svc, gw, runID := replayServiceFixture(t)
		gw.read = func(context.Context, event.ReadRequest) (event.Page, error) {
			t.Fatal("invalid request reached storage")
			return event.Page{}, nil
		}
		_, err := svc.GetTask(context.Background(), Credentials{Headers: headers}, GetTaskRequest{Tenant: "worker", ID: runID.String()})
		var protocolErr *Error
		if !errors.As(err, &protocolErr) || protocolErr.Code != CodeInvalidParams {
			t.Fatalf("bad input not rejected: %v", err)
		}
	}
}

func TestRemoteReplayDiscardsChangedPeerOrSnapshot(t *testing.T) {
	for _, mode := range []string{"revoked", "scope", "node", "revision", "foreign cursor", "denied"} {
		t.Run(mode, func(t *testing.T) {
			svc, gw, runID := replayServiceFixture(t)
			calls := 0
			svc.resolver = ResolverFunc(func(context.Context, Credentials) (Peer, error) {
				calls++
				peer := Peer{Node: "trusted", Scope: testScope()}
				if calls > 1 {
					switch mode {
					case "revoked":
						return Peer{}, errors.New("SECRET revocation")
					case "scope":
						peer.Scope.Levels[0].Value = "other"
					case "node":
						peer.Node = "other"
					}
				}
				return peer, nil
			})
			original := gw.read
			gw.read = func(ctx context.Context, r event.ReadRequest) (event.Page, error) {
				p, e := original(ctx, r)
				if mode == "revision" {
					p.Watermark.RunRevision++
				}
				if mode == "denied" {
					return event.Page{}, errors.Join(event.ErrAccessDenied, errors.New("SECRET cause"))
				}
				return p, e
			}
			cred := replayCredentials()
			if mode == "foreign cursor" {
				cred.Headers[ReplayCursorHeader] = []string{"1." + id.NewAgentRunID().String() + ".0"}
			}
			task, err := svc.GetTask(context.Background(), cred, GetTaskRequest{Tenant: "worker", ID: runID.String()})
			if err == nil || task != nil || strings.Contains(err.Error(), "SECRET") {
				t.Fatalf("unsafe replay: %v %v", task, err)
			}
		})
	}
}

func TestRemoteReplayCannotStartWork(t *testing.T) {
	svc, gw, _ := replayServiceFixture(t)
	_, err := svc.SendMessage(context.Background(), replayCredentials(), plainRequest("execute"))
	if err == nil || gw.calls() != 0 {
		t.Fatalf("replay negotiation started work: %v", err)
	}
}

func TestRemoteReplayRequiresConfiguredGateway(t *testing.T) {
	if NewService(newFakeGateway(), okResolver(), Options{PublicEventReplay: true}) != nil {
		t.Fatal("unsupported gateway accepted")
	}
	_, gw, _ := replayServiceFixture(t)
	gw.ready = false
	if NewService(gw, okResolver(), Options{PublicEventReplay: true}) != nil {
		t.Fatal("unconfigured gateway accepted")
	}
}

func TestRemoteReplayHTTPBindings(t *testing.T) {
	for _, binding := range []string{"rest", "rpc"} {
		t.Run(binding, func(t *testing.T) {
			svc, _, runID := replayServiceFixture(t)
			path, method, body := "/worker/tasks/"+runID.String(), http.MethodGet, ""
			handler := svc.RESTHandler()
			if binding == "rpc" {
				handler = svc.JSONRPCHandler()
				path = "/"
				method = http.MethodPost
				body = `{"jsonrpc":"2.0","id":1,"method":"GetTask","params":{"tenant":"worker","id":"` + runID.String() + `"}}`
			}
			request := httptest.NewRequestWithContext(t.Context(), method, path, strings.NewReader(body))
			request.Header.Set("A2A-Extensions", ReplayExtensionURI)
			recorder := httptest.NewRecorder()
			handler.ServeHTTP(recorder, request)
			if recorder.Code != 200 || recorder.Header().Get("A2A-Extensions") != ReplayExtensionURI || recorder.Header().Get("Cache-Control") != "no-store" || !strings.Contains(recorder.Body.String(), ReplayMetadataKey) || strings.Contains(recorder.Body.String(), "SECRET") {
				t.Fatalf("unsafe binding response: %d %v %s", recorder.Code, recorder.Header(), recorder.Body.String())
			}
		})
	}
}

func TestRemoteReplayGapAndMalformedStorage(t *testing.T) {
	for _, malformed := range []bool{false, true} {
		t.Run(strconv.FormatBool(malformed), func(t *testing.T) {
			svc, gw, runID := replayServiceFixture(t)
			gw.read = func(_ context.Context, _ event.ReadRequest) (event.Page, error) {
				p := event.Page{Watermark: event.Watermark{SchemaVersion: 1, RunID: runID, FirstSequence: 3, LastSequence: 3, RunRevision: 2}, Next: event.Cursor{SchemaVersion: 1, RunID: runID, Sequence: 2}}
				if malformed {
					p.Next.Sequence = 3
				}
				return p, event.ErrCursorGap
			}
			task, err := svc.GetTask(t.Context(), replayCredentials(), GetTaskRequest{Tenant: "worker", ID: runID.String()})
			if malformed {
				if err == nil || task != nil {
					t.Fatal("malformed gap accepted")
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			page := task.Metadata[ReplayMetadataKey].(ReplayPageMetadata)
			if !page.Gap || len(page.Events) != 0 || page.Watermark.FirstSequence != "3" || !strings.HasSuffix(page.NextCursor, ".2") {
				t.Fatalf("bad gap: %+v", page)
			}
		})
	}
}

func TestRemoteReplayRouteAndDelivery(t *testing.T) {
	svc, gw, runID := replayServiceFixture(t)
	for _, tenant := range []string{"", "missing"} {
		if task, err := svc.GetTask(t.Context(), replayCredentials(), GetTaskRequest{Tenant: tenant, ID: runID.String()}); task != nil || err == nil {
			t.Fatal("unknown route accepted")
		}
	}
	deliveryID := id.NewDeliveryID()
	gw.addDelivery(&a2a.Delivery{ID: deliveryID, RunID: runID})
	task, err := svc.GetTask(t.Context(), replayCredentials(), GetTaskRequest{Tenant: "worker", ID: deliveryID.String()})
	if err != nil || task.ID != runID.String() {
		t.Fatalf("delivery did not resolve: %v %v", task, err)
	}
	gw.addDelivery(&a2a.Delivery{ID: deliveryID})
	if task, err = svc.GetTask(t.Context(), replayCredentials(), GetTaskRequest{Tenant: "worker", ID: deliveryID.String()}); task != nil || err == nil {
		t.Fatal("unstarted delivery fabricated replay")
	}
	svc.opts.PublicEventReplay = false
	if task, err = svc.GetTask(t.Context(), replayCredentials(), GetTaskRequest{Tenant: "worker", ID: runID.String()}); task != nil || err == nil {
		t.Fatal("disabled replay fell back to content")
	}
}

func TestRemoteReplayPeerContextIsDetached(t *testing.T) {
	svc, _, _ := replayServiceFixture(t)
	scope := testScope()
	svc.resolver = ResolverFunc(func(context.Context, Credentials) (Peer, error) { return Peer{Node: "peer", Scope: scope}, nil })
	ctx, _, err := svc.authenticate(t.Context(), Credentials{})
	if err != nil {
		t.Fatal(err)
	}
	scope.Levels[0].Value = "mutated"
	peer, ok := PeerFromContext(ctx)
	if !ok || peer.Scope.Canonical() != testScope().Canonical() {
		t.Fatal("resolver scope alias")
	}
	peer.Scope.Levels[0].Value = "mutated"
	next, _ := PeerFromContext(ctx)
	if next.Scope.Canonical() != testScope().Canonical() {
		t.Fatal("reader scope alias")
	}
}

func TestRemoteReplayReadDeadline(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		svc, gw, runID := replayServiceFixture(t)
		gw.read = func(ctx context.Context, _ event.ReadRequest) (event.Page, error) {
			<-ctx.Done()
			return event.Page{}, ctx.Err()
		}
		start := time.Now()
		task, err := svc.GetTask(t.Context(), replayCredentials(), GetTaskRequest{Tenant: "worker", ID: runID.String()})
		if task != nil || err == nil || time.Since(start) != 5*time.Second {
			t.Fatalf("unbounded read: %v %v", task, err)
		}
	})
}

func TestRemoteReplayCannotCancelListOrSubscribe(t *testing.T) {
	svc, _, runID := replayServiceFixture(t)
	cred := replayCredentials()
	if _, err := svc.CancelTask(t.Context(), cred, CancelTaskRequest{Tenant: "worker", ID: runID.String()}); err == nil {
		t.Fatal("replay canceled work")
	}
	if _, err := svc.ListTasks(t.Context(), cred, ListTasksRequest{Tenant: "worker"}); err == nil {
		t.Fatal("replay listed content")
	}
	if err := svc.SubscribeTask(t.Context(), cred, "worker", runID.String(), func(StreamEvent) error { t.Fatal("replay started subscription"); return nil }); err == nil {
		t.Fatal("replay subscribed")
	}
}

func TestRemoteReplayCardReflectsService(t *testing.T) {
	svc, _, _ := replayServiceFixture(t)
	svc.opts.DefaultAgent = "worker"
	svc.opts.Exposed = []string{"worker"}
	svc.opts.Scope = testScope()
	for _, enabled := range []bool{true, false} {
		svc.opts.PublicEventReplay = enabled
		svc.opts.Card.PublicEventReplay = !enabled
		recorder := httptest.NewRecorder()
		svc.CardHandler().ServeHTTP(recorder, httptest.NewRequestWithContext(t.Context(), http.MethodGet, WellKnownCardPath, nil))
		if recorder.Code != 200 || strings.Contains(recorder.Body.String(), ReplayExtensionURI) != enabled {
			t.Fatalf("misleading capability: %s", recorder.Body.String())
		}
	}
}
