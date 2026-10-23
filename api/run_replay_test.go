package api

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/xraph/cortex"
	"github.com/xraph/cortex/engine"
	"github.com/xraph/cortex/event"
	"github.com/xraph/cortex/id"
	"github.com/xraph/cortex/store"
	"github.com/xraph/cortex/store/scopespy"
)

type replayTestStore struct {
	store.Store
	read func(context.Context, event.ReadRequest) (event.Page, error)
}

func (s replayTestStore) ReadPublicEvents(ctx context.Context, r event.ReadRequest) (event.Page, error) {
	return s.read(ctx, r)
}

type replayTestAuthorizer func(context.Context, id.AgentRunID) error

func (a replayTestAuthorizer) AuthorizePublicReplay(ctx context.Context, runID id.AgentRunID) error {
	return a(ctx, runID)
}

func TestReplayHTTPRoutes(t *testing.T) {
	runID := id.NewAgentRunID()
	for _, tc := range []struct {
		name, query                         string
		readErr, authErr                    error
		gap, corrupt, noScope, unconfigured bool
		want                                int
	}{
		{name: "page", want: 200}, {name: "resume", query: "?after=1." + runID.String() + ".1", want: 200},
		{name: "gap", gap: true, want: 410}, {name: "bad cursor", query: "?after=bad", want: 400},
		{name: "negative limit", query: "?limit=-1", want: 400}, {name: "oversized", query: "?limit=129", want: 400},
		{name: "foreign cursor", query: "?after=1." + id.NewAgentRunID().String() + ".0", want: 400},
		{name: "denied", authErr: errors.New("SECRET grant"), want: 403},
		{name: "missing scope", noScope: true, want: 403}, {name: "missing run", readErr: cortex.ErrRunNotFound, want: 404},
		{name: "storage failure", readErr: errors.New("SECRET storage"), want: 503},
		{name: "corrupt storage", corrupt: true, want: 503}, {name: "not configured", unconfigured: true, want: 503},
	} {
		t.Run(tc.name, func(t *testing.T) {
			reads, grants := 0, 0
			st := replayTestStore{Store: scopespy.New(), read: func(_ context.Context, r event.ReadRequest) (event.Page, error) {
				reads++
				page := event.Page{Watermark: event.Watermark{SchemaVersion: 1, RunID: runID, FirstSequence: 1, LastSequence: 1, RunRevision: 2}, Next: event.Cursor{SchemaVersion: 1, RunID: runID, Sequence: 1}}
				if r.After.Sequence == 0 {
					page.Events = []event.Event{{SchemaVersion: 1, RunID: runID, Sequence: 1, RunRevision: 2, ObservedAt: time.Now().UTC(), Input: event.Input{ID: id.New(event.PrefixEvent), Type: event.ExecutionFinished}}}
				}
				if tc.gap {
					page.Events = nil
					page.Watermark.FirstSequence = 2
					return page, event.ErrCursorGap
				}
				if tc.corrupt {
					page.Next.Sequence = 99
				}
				return page, tc.readErr
			}}
			opts := []engine.Option{engine.WithStore(st)}
			if !tc.unconfigured {
				opts = append(opts, engine.WithPublicEventReplay(replayTestAuthorizer(func(context.Context, id.AgentRunID) error { grants++; return tc.authErr })))
			}
			eng, err := engine.New(opts...)
			if err != nil {
				t.Fatal(err)
			}
			ctx := context.Background()
			if !tc.noScope {
				ctx = cortex.WithScope(ctx, cortex.Scope{Levels: []cortex.Level{{Key: "tenant", Value: "a"}}})
			}
			req := httptest.NewRequestWithContext(ctx, http.MethodGet, "/v1/runs/"+runID.String()+"/events"+tc.query, nil)
			res := httptest.NewRecorder()
			New(eng, nil).Handler().ServeHTTP(res, req)
			if res.Code != tc.want {
				t.Fatalf("status %d want %d: %s", res.Code, tc.want, res.Body)
			}
			if strings.Contains(res.Body.String(), "SECRET") {
				t.Fatalf("private error: %s", res.Body)
			}
			if tc.want == 400 && reads != 0 {
				t.Fatal("invalid request reached storage")
			}
			if tc.want == 200 || tc.want == 410 {
				if grants != 2 {
					t.Fatalf("grants %d", grants)
				}
				if !strings.Contains(res.Body.String(), "next_cursor") {
					t.Fatal("missing cursor")
				}
			}
			if tc.gap && !strings.Contains(res.Body.String(), `"gap":true`) {
				t.Fatal("missing gap")
			}
		})
	}
}
