package a2aremote

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"testing/synctest"
	"time"

	"github.com/xraph/cortex"
	"github.com/xraph/cortex/id"
	"github.com/xraph/cortex/run"
	"github.com/xraph/cortex/safety"
	"github.com/xraph/cortex/verification"
)

func TestRemoteContentDoesNotFallBackToRawOutput(t *testing.T) {
	svc, _, runID := replayServiceFixture(t)
	cred := Credentials{Headers: map[string][]string{
		"A2A-Extensions":   {"https://cortex.xraph.dev/a2a/extensions/result-content/v1"},
		"Cortex-Result-ID": {"vresult_invalid"},
	}}
	task, err := svc.GetTask(t.Context(), cred, GetTaskRequest{Tenant: "worker", ID: runID.String()})
	body, _ := json.Marshal(task)
	if err == nil || task != nil || strings.Contains(string(body), "SECRET") {
		t.Fatalf("content request fell back to raw task: %s, %v", body, err)
	}
}

type contentGateway struct {
	*fakeGateway
	ready bool
	read  func(context.Context, verification.ResultContentRequest) (verification.PublishedResult, error)
}

func (g *contentGateway) ResultContentAvailable() bool { return g.ready }
func (g *contentGateway) ReadResultContent(ctx context.Context, req verification.ResultContentRequest) (verification.PublishedResult, error) {
	return g.read(ctx, req)
}

func contentFixture(t *testing.T) (*Service, *contentGateway, Credentials, GetTaskRequest) {
	t.Helper()
	base := newFakeGateway()
	runID, resultID := id.NewAgentRunID(), id.New(verification.PrefixResult)
	base.addRun(&run.Run{ID: runID, AgentID: base.agents["worker"].ID, Revision: 9007199254740993, State: run.StateCompleted, Output: "SECRET candidate", Error: "SECRET error"})
	gw := &contentGateway{fakeGateway: base, ready: true}
	gw.read = func(ctx context.Context, req verification.ResultContentRequest) (verification.PublishedResult, error) {
		if peer, ok := PeerFromContext(ctx); !ok || peer.Node == "" || peer.Scope.Canonical() != cortex.ScopeFromContext(ctx).Canonical() {
			t.Error("missing peer context")
		}
		return verification.PublishedResult{SchemaVersion: 1, RunID: req.RunID, ResultID: req.ResultID, RunRevision: 9007199254740993, Content: "screened", PublicationStatus: safety.StatusRedacted, ContentChanged: true, VerificationStatus: verification.StatusPassed, VerifiedAt: time.Now(), CheckedAt: time.Now()}, nil
	}
	svc := NewService(gw, okResolver(), Options{ResultContent: true})
	if svc == nil {
		t.Fatal("missing content service")
	}
	return svc, gw, Credentials{Headers: map[string][]string{"A2A-Extensions": {ResultContentExtensionURI}, ResultIDHeader: {resultID.String()}}}, GetTaskRequest{Tenant: "worker", ID: runID.String()}
}

func TestRemoteContentProjection(t *testing.T) {
	svc, _, cred, req := contentFixture(t)
	task, err := svc.GetTask(t.Context(), cred, req)
	if err != nil {
		t.Fatal(err)
	}
	body, _ := json.Marshal(task)
	if !ResultContentActive(task) || len(task.Artifacts) != 0 || len(task.History) != 0 || task.Status.Message != nil || strings.Contains(string(body), "SECRET") || !strings.Contains(string(body), `"run_revision":"9007199254740993"`) || !strings.Contains(string(body), `"content":"screened"`) {
		t.Fatalf("unsafe projection: %s", body)
	}
}

func TestRemoteContentRejectsInvalidNegotiation(t *testing.T) {
	for _, mode := range []string{"missing extension", "missing result", "duplicate", "case duplicate", "empty", "oversized", "replay", "cursor", "bad result"} {
		t.Run(mode, func(t *testing.T) {
			svc, gw, cred, req := contentFixture(t)
			gw.read = func(context.Context, verification.ResultContentRequest) (verification.PublishedResult, error) {
				t.Fatal("invalid request reached content reader")
				return verification.PublishedResult{}, nil
			}
			switch mode {
			case "missing extension":
				delete(cred.Headers, "A2A-Extensions")
			case "missing result":
				delete(cred.Headers, ResultIDHeader)
			case "duplicate":
				cred.Headers[ResultIDHeader] = []string{"one", "two"}
			case "case duplicate":
				cred.Headers[strings.ToLower(ResultIDHeader)] = cred.Headers[ResultIDHeader]
			case "empty":
				cred.Headers[ResultIDHeader] = []string{""}
			case "oversized":
				cred.Headers[ResultIDHeader] = []string{strings.Repeat("x", 129)}
			case "replay":
				cred.Headers["A2A-Extensions"] = []string{ResultContentExtensionURI + "," + ReplayExtensionURI}
			case "cursor":
				cred.Headers[ReplayCursorHeader] = []string{"cursor"}
			case "bad result":
				cred.Headers[ResultIDHeader] = []string{"vresult_invalid"}
			}
			task, err := svc.GetTask(t.Context(), cred, req)
			if err == nil || task != nil {
				t.Fatalf("accepted invalid content request: %v", task)
			}
		})
	}
}

func TestRemoteContentFailsClosed(t *testing.T) {
	for _, mode := range []string{"disabled", "unready", "tenant", "read denial", "provider", "revoked", "scope", "node", "revision", "run", "result", "unscanned", "changed approved", "oversized", "invalid utf8", "deadline", "cancelled"} {
		t.Run(mode, func(t *testing.T) {
			svc, gw, cred, req := contentFixture(t)
			calls := 0
			svc.resolver = ResolverFunc(func(context.Context, Credentials) (Peer, error) {
				calls++
				peer := Peer{Node: "trusted", Scope: testScope()}
				if calls > 1 {
					switch mode {
					case "revoked":
						return Peer{}, errors.New("SECRET revoked")
					case "scope":
						peer.Scope.Levels[0].Value = "other"
					case "node":
						peer.Node = "other"
					}
				}
				return peer, nil
			})
			original := gw.read
			gw.read = func(ctx context.Context, request verification.ResultContentRequest) (verification.PublishedResult, error) {
				result, err := original(ctx, request)
				switch mode {
				case "read denial":
					return result, errors.Join(verification.ErrResultAccessDenied, errors.New("SECRET denied"))
				case "provider":
					return result, errors.New("SECRET provider")
				case "revision":
					result.RunRevision++
				case "run":
					result.RunID = id.NewAgentRunID()
				case "result":
					result.ResultID = id.New(verification.PrefixResult)
				case "unscanned":
					result.PublicationStatus = safety.StatusUnscanned
				case "changed approved":
					result.PublicationStatus = safety.StatusApproved
				case "oversized":
					result.Content = strings.Repeat("x", 1<<20+1)
				case "invalid utf8":
					result.Content = string([]byte{0xff})
				case "deadline":
					<-ctx.Done()
				}
				return result, err
			}
			switch mode {
			case "disabled":
				svc.opts.ResultContent = false
			case "unready":
				gw.ready = false
			case "tenant":
				req.Tenant = "missing"
			}
			check := func(ctx context.Context) {
				task, err := svc.GetTask(ctx, cred, req)
				if err == nil || task != nil || strings.Contains(err.Error(), "SECRET") {
					t.Fatalf("unsafe result: %v %v", task, err)
				}
				if svc.contentReads.Load() != 0 {
					t.Fatal("read admission leaked")
				}
			}
			if mode == "deadline" {
				synctest.Test(t, func(t *testing.T) { check(t.Context()) })
				return
			}
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			if mode == "cancelled" {
				cancel()
			}
			check(ctx)
		})
	}
}

func TestRemoteContentCannotStartOrSubscribe(t *testing.T) {
	svc, gw, cred, req := contentFixture(t)
	_, sendErr := svc.SendMessage(t.Context(), cred, plainRequest("execute"))
	_, listErr := svc.ListTasks(t.Context(), cred, ListTasksRequest{Tenant: req.Tenant})
	_, cancelErr := svc.CancelTask(t.Context(), cred, CancelTaskRequest{Tenant: req.Tenant, ID: req.ID})
	subErr := svc.SubscribeTask(t.Context(), cred, req.Tenant, req.ID, func(StreamEvent) error { t.Fatal("content subscription emitted"); return nil })
	if sendErr == nil || listErr == nil || cancelErr == nil || subErr == nil || gw.calls() != 0 {
		t.Fatal("content negotiation reached another operation")
	}
}

func TestRemoteContentRequiresGatewayAndCardOptIn(t *testing.T) {
	var absent *contentGateway
	if NewService(absent, okResolver(), Options{ResultContent: true}) != nil {
		t.Fatal("typed nil gateway accepted")
	}
	if NewService(newFakeGateway(), okResolver(), Options{ResultContent: true}) != nil {
		t.Fatal("accepted missing gateway")
	}
	svc, gw, _, _ := contentFixture(t)
	gw.ready = false
	if NewService(gw, okResolver(), Options{ResultContent: true}) != nil {
		t.Fatal("accepted unconfigured gateway")
	}
	for _, enabled := range []bool{false, true} {
		svc.opts.ResultContent = enabled
		svc.opts.Card.ResultContent = !enabled
		svc.opts.Exposed = []string{"worker"}
		svc.opts.Scope = testScope()
		svc.opts.DefaultAgent = "worker"
		recorder := httptest.NewRecorder()
		svc.CardHandler().ServeHTTP(recorder, httptest.NewRequestWithContext(t.Context(), http.MethodGet, WellKnownCardPath, nil))
		if recorder.Code != 200 || strings.Contains(recorder.Body.String(), ResultContentExtensionURI) != enabled {
			t.Fatalf("card drift: %s", recorder.Body.String())
		}
	}
}
