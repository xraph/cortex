package a2aremote

import (
	"bufio"
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"testing/synctest"
	"time"

	"github.com/xraph/cortex/event"
	"github.com/xraph/cortex/run"
)

type replayDeadlineWriter struct {
	*httptest.ResponseRecorder
	deadline time.Time
	stalled  bool
	flushErr error
}

func (w *replayDeadlineWriter) SetWriteDeadline(at time.Time) error { w.deadline = at; return nil }
func (w *replayDeadlineWriter) Write(b []byte) (int, error) {
	if w.stalled {
		time.Sleep(time.Until(w.deadline))
		return 0, os.ErrDeadlineExceeded
	}
	return w.ResponseRecorder.Write(b)
}
func (w *replayDeadlineWriter) FlushError() error { return w.flushErr }

func TestRemoteReplayHTTPSubscriptionFraming(t *testing.T) {
	for _, binding := range []string{"rest", "rpc"} {
		t.Run(binding, func(t *testing.T) {
			svc, gw, runID := replayServiceFixture(t)
			svc.opts.Streaming = true
			current, _ := gw.GetRun(t.Context(), runID)
			current.State = run.StateRunning
			gw.addRun(current)
			reads := 0
			original := gw.read
			gw.read = func(ctx context.Context, req event.ReadRequest) (event.Page, error) {
				reads++
				if reads == 1 && req.After.Sequence != 1 {
					t.Fatal("Last-Event-ID did not win")
				}
				if reads == 2 {
					return event.Page{}, event.ErrAccessDenied
				}
				return original(ctx, req)
			}
			req := httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/worker/tasks/"+runID.String()+":subscribe", nil)
			handler := svc.RESTHandler()
			if binding == "rpc" {
				req = httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/", strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"SubscribeToTask","params":{"tenant":"worker","id":"`+runID.String()+`"}}`))
				handler = svc.JSONRPCHandler()
			}
			req.Header.Set("A2A-Extensions", ReplayExtensionURI)
			req.Header.Set(ReplayCursorHeader, "1."+runID.String()+".0")
			req.Header.Set("Last-Event-ID", "1."+runID.String()+".1")
			writer := &replayDeadlineWriter{ResponseRecorder: httptest.NewRecorder()}
			handler.ServeHTTP(writer, req)
			body := writer.Body.String()
			if writer.Header().Get("A2A-Extensions") != ReplayExtensionURI || writer.Header().Get("Cache-Control") != "no-store" || !strings.Contains(body, "id: 1."+runID.String()+".1\n") || !strings.Contains(body, `"task":`) || strings.Contains(body, "SECRET") || strings.Contains(body, `"final":true`) || reads != 2 {
				t.Fatalf("bad framing: %s %v", body, writer.Header())
			}
			if binding == "rpc" && !strings.Contains(body, `"jsonrpc":"2.0"`) {
				t.Fatal("lost RPC envelope")
			}
			if svc.replayStreams.Load() != 0 {
				t.Fatal("slot leaked")
			}
		})
	}
}

func TestRemoteReplayHTTPPreflightAndGap(t *testing.T) {
	for _, mode := range []string{"denied", "terminal", "unsupported", "no flush", "gap", "flush"} {
		t.Run(mode, func(t *testing.T) {
			svc, gw, runID := replayServiceFixture(t)
			svc.opts.Streaming = true
			current, _ := gw.GetRun(t.Context(), runID)
			if mode != "terminal" {
				current.State = run.StateRunning
			}
			gw.addRun(current)
			if mode == "denied" {
				svc.resolver = ResolverFunc(func(context.Context, Credentials) (Peer, error) { return Peer{}, errors.New("SECRET") })
			}
			if mode == "gap" {
				gw.read = func(context.Context, event.ReadRequest) (event.Page, error) {
					return event.Page{Watermark: event.Watermark{SchemaVersion: 1, RunID: runID, FirstSequence: 3, LastSequence: 2, RunRevision: 2}, Next: event.Cursor{SchemaVersion: 1, RunID: runID, Sequence: 2}}, event.ErrCursorGap
				}
			}
			base := httptest.NewRecorder()
			writer := &replayDeadlineWriter{ResponseRecorder: base}
			if mode == "flush" {
				writer.flushErr = errors.New("socket")
			}
			var response http.ResponseWriter = writer
			if mode == "unsupported" {
				response = base
			}
			if mode == "no flush" {
				response = &noFlushReplayWriter{writer: writer}
			}
			req := httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/worker/tasks/"+runID.String()+":subscribe", nil)
			req.Header.Set("A2A-Extensions", ReplayExtensionURI)
			svc.RESTHandler().ServeHTTP(response, req)
			body := base.Body.String()
			if strings.Contains(body, "SECRET") {
				t.Fatal("content leaked")
			}
			if mode == "gap" {
				if !strings.Contains(body, `"gap":true`) || strings.Contains(body, "id: ") {
					t.Fatalf("gap advanced cursor: %s", body)
				}
			} else if mode != "flush" && (base.Code == 200 || strings.Contains(body, "data: ")) {
				t.Fatalf("preflight streamed: %d %s", base.Code, body)
			}
			if svc.replayStreams.Load() != 0 {
				t.Fatal("slot leaked")
			}
		})
	}
}

func TestRemoteReplayHTTPStalledWriter(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		svc, gw, runID := replayServiceFixture(t)
		svc.opts.Streaming = true
		current, _ := gw.GetRun(t.Context(), runID)
		current.State = run.StateRunning
		gw.addRun(current)
		writer := &replayDeadlineWriter{ResponseRecorder: httptest.NewRecorder(), stalled: true}
		req := httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/worker/tasks/"+runID.String()+":subscribe", nil)
		req.Header.Set("A2A-Extensions", ReplayExtensionURI)
		start := time.Now()
		svc.RESTHandler().ServeHTTP(writer, req)
		if time.Since(start) != 10*time.Second || svc.replayStreams.Load() != 0 {
			t.Fatal("unbounded socket write")
		}
	})
}

func TestRemoteReplayHTTPDisconnect(t *testing.T) {
	svc, gw, runID := replayServiceFixture(t)
	svc.opts.Streaming = true
	current, _ := gw.GetRun(t.Context(), runID)
	current.State = run.StateRunning
	gw.addRun(current)
	handler := svc.RESTHandler()
	done := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { defer close(done); handler.ServeHTTP(w, r) }))
	defer server.Close()
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, server.URL+"/worker/tasks/"+runID.String()+":subscribe", nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("A2A-Extensions", ReplayExtensionURI)
	response, err := server.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	if _, err = bufio.NewReader(response.Body).ReadString('\n'); err != nil {
		t.Fatal(err)
	}
	response.Body.Close()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("subscription outlived disconnect")
	}
	if svc.replayStreams.Load() != 0 {
		t.Fatal("slot leaked")
	}
}

type noFlushReplayWriter struct{ writer *replayDeadlineWriter }

func (w *noFlushReplayWriter) Header() http.Header            { return w.writer.Header() }
func (w *noFlushReplayWriter) WriteHeader(code int)           { w.writer.WriteHeader(code) }
func (w *noFlushReplayWriter) Write(body []byte) (int, error) { return w.writer.Write(body) }
func (w *noFlushReplayWriter) SetWriteDeadline(at time.Time) error {
	return w.writer.SetWriteDeadline(at)
}
