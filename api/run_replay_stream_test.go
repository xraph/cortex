package api

import (
	"bufio"
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/xraph/cortex"
	"github.com/xraph/cortex/engine"
	"github.com/xraph/cortex/event"
	"github.com/xraph/cortex/id"
	"github.com/xraph/cortex/store/scopespy"
)

type deadlineRecorder struct {
	*httptest.ResponseRecorder
	deadlines []time.Time
	writeErr  error
}

func (r *deadlineRecorder) SetWriteDeadline(at time.Time) error {
	r.deadlines = append(r.deadlines, at)
	return nil
}
func (r *deadlineRecorder) Write(body []byte) (int, error) {
	if r.writeErr != nil {
		return 0, r.writeErr
	}
	return r.ResponseRecorder.Write(body)
}

func streamReplayEngine(t *testing.T, read func(context.Context, event.ReadRequest) (event.Page, error), auth replayTestAuthorizer) *engine.Engine {
	t.Helper()
	e, err := engine.New(engine.WithStore(replayTestStore{Store: scopespy.New(), read: read}), engine.WithPublicEventReplay(auth))
	if err != nil {
		t.Fatal(err)
	}
	return e
}
func streamRequest(ctx context.Context, runID id.AgentRunID, query string) *http.Request {
	scope := cortex.Scope{Levels: []cortex.Level{{Key: "tenant", Value: "replay"}}}
	return httptest.NewRequestWithContext(cortex.WithScope(ctx, scope), http.MethodGet, "/v1/runs/"+runID.String()+"/events/stream"+query, nil)
}
func emptyReplayPage(r event.ReadRequest) event.Page {
	return event.Page{Watermark: event.Watermark{SchemaVersion: 1, RunID: r.After.RunID, FirstSequence: r.After.Sequence + 1, LastSequence: r.After.Sequence, RunRevision: 1}, Next: r.After}
}

func TestReplaySSEPreflightAndCursor(t *testing.T) {
	runID := id.NewAgentRunID()
	for _, tc := range []struct {
		name, query, lastID string
		denied, unsupported bool
		want                int
	}{
		{name: "invalid", query: "?after=bad", want: 400},
		{name: "foreign last ID", lastID: "1." + id.NewAgentRunID().String() + ".1", want: 400},
		{name: "denied", denied: true, want: 403},
		{name: "unsupported deadline", unsupported: true, want: 503},
	} {
		t.Run(tc.name, func(t *testing.T) {
			reads := 0
			eng := streamReplayEngine(t, func(_ context.Context, r event.ReadRequest) (event.Page, error) {
				reads++
				return emptyReplayPage(r), nil
			}, func(context.Context, id.AgentRunID) error {
				if tc.denied {
					return errors.New("SECRET authorization")
				}
				return nil
			})
			recorder := httptest.NewRecorder()
			var writer http.ResponseWriter = &deadlineRecorder{ResponseRecorder: recorder}
			if tc.unsupported {
				writer = recorder
			}
			req := streamRequest(context.Background(), runID, tc.query)
			req.Header.Set("Last-Event-ID", tc.lastID)
			New(eng, nil).Handler().ServeHTTP(writer, req)
			if recorder.Code != tc.want {
				t.Fatalf("got %d want %d: %s", recorder.Code, tc.want, recorder.Body)
			}
			if strings.Contains(recorder.Header().Get("Content-Type"), "event-stream") || strings.Contains(recorder.Body.String(), "SECRET") {
				t.Fatalf("bad preflight: %v %s", recorder.Header(), recorder.Body)
			}
			if tc.want == 400 && reads != 0 {
				t.Fatal("invalid cursor reached storage")
			}
		})
	}
}

func TestReplaySSEResumeHeartbeatAndRevocation(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		runID := id.NewAgentRunID()
		token := "1." + runID.String() + ".5"
		reads, auths := 0, 0
		eng := streamReplayEngine(t, func(_ context.Context, r event.ReadRequest) (event.Page, error) {
			reads++
			if r.After.Sequence != 5 {
				t.Fatalf("did not use Last-Event-ID: %v", r)
			}
			return emptyReplayPage(r), nil
		}, func(context.Context, id.AgentRunID) error {
			auths++
			if auths == 3 {
				return errors.New("SECRET revoked")
			}
			return nil
		})
		res := &deadlineRecorder{ResponseRecorder: httptest.NewRecorder()}
		req := streamRequest(context.Background(), runID, "?after=1."+runID.String()+".0&limit=2")
		req.Header.Set("Last-Event-ID", token)
		New(eng, nil).Handler().ServeHTTP(res, req)
		body := res.Body.String()
		if res.Code != 200 || !strings.Contains(body, "id: "+token+"\n") || !strings.Contains(body, "event: heartbeat\n") || !strings.Contains(body, "event: error\n") || !strings.Contains(body, "replay_denied") || strings.Contains(body, "SECRET") {
			t.Fatalf("bad SSE: %d %s", res.Code, body)
		}
		if reads != 1 || auths != 3 {
			t.Fatalf("read %d auth %d", reads, auths)
		}
		if len(res.deadlines) < 2 || res.deadlines[0].IsZero() {
			t.Fatal("missing write deadline")
		}
	})
}

func TestReplaySSEGapDoesNotAdvanceBrowserCursor(t *testing.T) {
	runID := id.NewAgentRunID()
	reads := 0
	eng := streamReplayEngine(t, func(_ context.Context, r event.ReadRequest) (event.Page, error) {
		reads++
		p := emptyReplayPage(r)
		p.Watermark.FirstSequence = 3
		p.Watermark.LastSequence = 2
		p.Next.Sequence = 2
		return p, event.ErrCursorGap
	}, func(context.Context, id.AgentRunID) error { return nil })
	res := &deadlineRecorder{ResponseRecorder: httptest.NewRecorder()}
	New(eng, nil).Handler().ServeHTTP(res, streamRequest(context.Background(), runID, ""))
	if reads != 1 || !strings.Contains(res.Body.String(), "event: gap\n") || strings.Contains(res.Body.String(), "\nid: ") {
		t.Fatalf("bad gap: %s", res.Body)
	}
}

func TestReplaySSEWriterFailureStopsReads(t *testing.T) {
	runID := id.NewAgentRunID()
	reads := 0
	eng := streamReplayEngine(t, func(_ context.Context, r event.ReadRequest) (event.Page, error) {
		reads++
		return emptyReplayPage(r), nil
	}, func(context.Context, id.AgentRunID) error { return nil })
	res := &deadlineRecorder{ResponseRecorder: httptest.NewRecorder(), writeErr: errors.New("socket disconnected")}
	ctx := context.Background()
	api := New(eng, nil)
	api.Handler().ServeHTTP(res, streamRequest(ctx, runID, ""))
	if reads != 1 || res.Body.Len() != 0 {
		t.Fatalf("read %d body %s", reads, res.Body)
	}
	if api.replayStreams.Load() != 0 {
		t.Fatal("subscription slot leaked")
	}
	if ctx.Err() != nil {
		t.Fatal("cancelled parent")
	}
}

func TestReplaySSEHTTPDisconnect(t *testing.T) {
	runID := id.NewAgentRunID()
	var reads atomic.Int64
	eng := streamReplayEngine(t, func(_ context.Context, r event.ReadRequest) (event.Page, error) {
		reads.Add(1)
		return emptyReplayPage(r), nil
	}, func(context.Context, id.AgentRunID) error { return nil })
	api := New(eng, nil)
	handler := api.Handler()
	done := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer close(done)
		scope := cortex.Scope{Levels: []cortex.Level{{Key: "tenant", Value: "socket"}}}
		handler.ServeHTTP(w, r.WithContext(cortex.WithScope(r.Context(), scope)))
	}))
	defer server.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, server.URL+"/v1/runs/"+runID.String()+"/events/stream", nil)
	if err != nil {
		t.Fatal(err)
	}
	response, err := server.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	line, err := bufio.NewReader(response.Body).ReadString('\n')
	if err != nil {
		t.Fatal(err)
	}
	if response.StatusCode != 200 || line != "event: heartbeat\n" {
		t.Fatalf("stream not established: %d %q", response.StatusCode, line)
	}
	if err = response.Body.Close(); err != nil {
		t.Fatal(err)
	}
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("subscription survived socket disconnect")
	}
	if api.replayStreams.Load() != 0 || reads.Load() < 1 {
		t.Fatalf("leaked subscription: slots %d reads %d", api.replayStreams.Load(), reads.Load())
	}
}

type stalledReplayWriter struct{ *deadlineRecorder }

func (r *stalledReplayWriter) Write([]byte) (int, error) {
	deadline := r.deadlines[len(r.deadlines)-1]
	if deadline.IsZero() {
		return 0, errors.New("missing deadline")
	}
	time.Sleep(time.Until(deadline))
	return 0, os.ErrDeadlineExceeded
}

func TestReplaySSEStalledWriterIsBounded(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		runID := id.NewAgentRunID()
		reads := 0
		eng := streamReplayEngine(t, func(_ context.Context, r event.ReadRequest) (event.Page, error) {
			reads++
			return emptyReplayPage(r), nil
		}, func(context.Context, id.AgentRunID) error { return nil })
		res := &stalledReplayWriter{deadlineRecorder: &deadlineRecorder{ResponseRecorder: httptest.NewRecorder()}}
		api := New(eng, nil)
		started := time.Now()
		api.Handler().ServeHTTP(res, streamRequest(context.Background(), runID, ""))
		if time.Since(started) != 10*time.Second || reads != 1 || api.replayStreams.Load() != 0 {
			t.Fatalf("stalled writer: elapsed %s reads %d slots %d", time.Since(started), reads, api.replayStreams.Load())
		}
	})
}

func TestReplaySSECapacityAndCancellationUnderLoad(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		runID := id.NewAgentRunID()
		var reads atomic.Int64
		eng := streamReplayEngine(t, func(ctx context.Context, _ event.ReadRequest) (event.Page, error) {
			reads.Add(1)
			<-ctx.Done()
			return event.Page{}, ctx.Err()
		}, func(context.Context, id.AgentRunID) error { return nil })
		api := New(eng, nil)
		handler := api.Handler()
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		done := make(chan struct{}, maxReplayStreams)
		for range maxReplayStreams {
			go func() {
				res := &deadlineRecorder{ResponseRecorder: httptest.NewRecorder()}
				handler.ServeHTTP(res, streamRequest(ctx, runID, ""))
				done <- struct{}{}
			}()
		}
		synctest.Wait()
		if api.replayStreams.Load() != maxReplayStreams {
			t.Fatalf("slots %d", api.replayStreams.Load())
		}
		busy := &deadlineRecorder{ResponseRecorder: httptest.NewRecorder()}
		handler.ServeHTTP(busy, streamRequest(ctx, runID, ""))
		if busy.Code != 503 || !strings.Contains(busy.Body.String(), "replay_busy") {
			t.Fatalf("capacity: %d %s", busy.Code, busy.Body)
		}
		if reads.Load() != maxReplayStreams {
			t.Fatalf("capacity exceeded: %d reads", reads.Load())
		}
		cancel()
		for range maxReplayStreams {
			<-done
		}
		if api.replayStreams.Load() != 0 {
			t.Fatalf("slots leaked: %d", api.replayStreams.Load())
		}
	})
}

type failedReplayFlush struct{ *deadlineRecorder }

func (*failedReplayFlush) FlushError() error { return errors.New("flush failed") }

func TestReplaySSEFlushFailureStopsWithoutErrorFrame(t *testing.T) {
	runID := id.NewAgentRunID()
	reads := 0
	eng := streamReplayEngine(t, func(_ context.Context, r event.ReadRequest) (event.Page, error) {
		reads++
		return emptyReplayPage(r), nil
	}, func(context.Context, id.AgentRunID) error { return nil })
	res := &failedReplayFlush{deadlineRecorder: &deadlineRecorder{ResponseRecorder: httptest.NewRecorder()}}
	api := New(eng, nil)
	api.Handler().ServeHTTP(res, streamRequest(context.Background(), runID, ""))
	if reads != 1 || strings.Count(res.Body.String(), "event:") != 1 || api.replayStreams.Load() != 0 {
		t.Fatalf("flush failure: %d %s", reads, res.Body)
	}
}

func TestReplaySSEPageReconnectDoesNotRepeatConsumedEvents(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		runID := id.NewAgentRunID()
		eventID := id.New(event.PrefixEvent)
		reads := 0
		eng := streamReplayEngine(t, func(_ context.Context, r event.ReadRequest) (event.Page, error) {
			reads++
			if reads%2 == 0 {
				return event.Page{}, event.ErrUnavailable
			}
			page := event.Page{Watermark: event.Watermark{SchemaVersion: 1, RunID: runID, FirstSequence: 1, LastSequence: 1, RunRevision: 2}, Next: event.Cursor{SchemaVersion: 1, RunID: runID, Sequence: 1}}
			if r.After.Sequence == 0 {
				page.Events = []event.Event{{Input: event.Input{ID: eventID, Type: event.ExecutionFinished}, SchemaVersion: 1, RunID: runID, Sequence: 1, RunRevision: 2, ObservedAt: time.Now().UTC()}}
			}
			return page, nil
		}, func(context.Context, id.AgentRunID) error { return nil })
		api := New(eng, nil)
		handler := api.Handler()
		first := &deadlineRecorder{ResponseRecorder: httptest.NewRecorder()}
		handler.ServeHTTP(first, streamRequest(context.Background(), runID, ""))
		cursor := "1." + runID.String() + ".1"
		if !strings.Contains(first.Body.String(), "event: page\n") || !strings.Contains(first.Body.String(), "id: "+cursor+"\n") || strings.Count(first.Body.String(), eventID.String()) != 1 {
			t.Fatalf("bad page: %s", first.Body)
		}
		req := streamRequest(context.Background(), runID, "")
		req.Header.Set("Last-Event-ID", cursor)
		second := &deadlineRecorder{ResponseRecorder: httptest.NewRecorder()}
		handler.ServeHTTP(second, req)
		if !strings.Contains(second.Body.String(), "event: heartbeat\n") || strings.Contains(second.Body.String(), eventID.String()) {
			t.Fatalf("repeated event: %s", second.Body)
		}
	})
}
