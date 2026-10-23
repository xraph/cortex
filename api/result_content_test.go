package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strconv"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/stretchr/testify/require"
	"github.com/xraph/forge"
	"github.com/xraph/grove"
	"github.com/xraph/grove/drivers/sqlitedriver"
	_ "github.com/xraph/grove/drivers/sqlitedriver/sqlitemigrate"

	"github.com/xraph/cortex"
	"github.com/xraph/cortex/engine"
	"github.com/xraph/cortex/evidence"
	"github.com/xraph/cortex/id"
	"github.com/xraph/cortex/internal/resulttest"
	"github.com/xraph/cortex/safety"
	"github.com/xraph/cortex/store/sqlite"
	"github.com/xraph/cortex/verification"
)

func contentStore(t *testing.T) *sqlite.Store {
	t.Helper()
	drv := sqlitedriver.New()
	require.NoError(t, drv.Open(context.Background(), filepath.Join(t.TempDir(), "content.db")))
	db, err := grove.Open(drv)
	require.NoError(t, err)
	s := sqlite.New(db)
	require.NoError(t, s.Migrate(context.Background()))
	t.Cleanup(func() { _ = s.Close() })
	return s
}

func TestResultContentHTTP(t *testing.T) {
	for _, mode := range []string{"success", "denied", "revoked", "deleted", "wrong scope", "no scope", "bad run", "bad result", "missing", "foreign run", "limit", "blocked", "scanner error", "unconfigured", "no deadline"} {
		t.Run(mode, func(t *testing.T) {
			st := contentStore(t)
			ctx := cortex.WithScope(context.Background(), cortex.Scope{Levels: []cortex.Level{{Key: "tenant", Value: "one"}}})
			receipt := resulttest.Commit(ctx, t, st)
			revoked := false
			checks := 0
			opts := append([]engine.Option{engine.WithStore(st)}, resulttest.Options(resulttest.Authorizer(func(context.Context, verification.ResultContentRequest) error {
				if mode == "denied" || revoked {
					return errors.New("SECRET grant")
				}
				return nil
			}), resulttest.Gate(func(_ context.Context, _ safety.PublicationRequest) (safety.PublicationDecision, error) {
				checks++
				switch mode {
				case "revoked":
					revoked = true
				case "deleted":
					require.NoError(t, st.TombstoneEvidence(evidence.WithDeletionActor(ctx, "operator"), receipt.Request.Current.Candidate, "removed"))
				case "blocked":
					return safety.PublicationDecision{}, safety.ErrBlocked
				case "scanner error":
					return safety.PublicationDecision{}, errors.New("SECRET provider")
				}
				return safety.PublicationDecision{Text: "public projection", Status: safety.StatusRedacted}, nil
			}))...)
			if mode == "limit" {
				opts = append(opts, engine.WithPublicationPolicy(safety.Policy{MaxOutputBytes: 1}))
			}
			if mode == "unconfigured" {
				opts = opts[:1]
			}
			eng, err := engine.New(opts...)
			require.NoError(t, err)
			runID, resultID := receipt.Request.Current.RunID.String(), receipt.Request.ResultID.String()
			requestCtx := ctx
			want := 200
			switch mode {
			case "denied", "revoked", "no scope", "blocked":
				want = 403
			case "deleted", "scanner error", "unconfigured", "no deadline":
				want = 503
			case "foreign run":
				want = 409
			case "limit":
				want = 413
			case "wrong scope", "missing":
				want = 404
			case "bad run", "bad result":
				want = 400
			}
			switch mode {
			case "no scope":
				requestCtx = context.Background()
			case "wrong scope":
				requestCtx = cortex.WithScope(ctx, cortex.Scope{Levels: []cortex.Level{{Key: "tenant", Value: "other"}}})
			case "bad run":
				runID = "bad"
			case "bad result":
				resultID = id.NewAgentID().String()
			case "foreign run":
				runID = id.NewAgentRunID().String()
			case "missing":
				resultID = id.New(verification.PrefixResult).String()
			}
			recorder := httptest.NewRecorder()
			var writer http.ResponseWriter = &deadlineRecorder{ResponseRecorder: recorder}
			if mode == "no deadline" {
				writer = recorder
			}
			New(eng, nil).Handler().ServeHTTP(writer, httptest.NewRequestWithContext(requestCtx, http.MethodGet, "/v1/runs/"+runID+"/results/"+resultID, nil))
			require.Equal(t, want, recorder.Code, recorder.Body.String())
			require.Equal(t, "no-store", recorder.Header().Get("Cache-Control"))
			require.NotContains(t, recorder.Body.String(), "SECRET")
			if want == 200 {
				var got verification.ResultContentResponse
				require.NoError(t, json.Unmarshal(recorder.Body.Bytes(), &got))
				require.Equal(t, "public projection", got.Content)
				require.True(t, got.ContentChanged)
				require.Equal(t, receipt.Request.ResultID, got.ResultID)
				require.Equal(t, strconv.FormatInt(receipt.RunRevision, 10), got.RunRevision)
			} else {
				require.Contains(t, recorder.Body.String(), "result_")
			}
			if mode == "denied" || mode == "no deadline" || mode == "bad run" || mode == "bad result" {
				require.Zero(t, checks)
			}
		})
	}
}

func TestResultContentStalledWriteIsBounded(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		st := contentStore(t)
		ctx := cortex.WithScope(context.Background(), cortex.Scope{Levels: []cortex.Level{{Key: "tenant", Value: "one"}}})
		receipt := resulttest.Commit(ctx, t, st)
		opts := append([]engine.Option{engine.WithStore(st)}, resulttest.Options(resulttest.Authorizer(func(context.Context, verification.ResultContentRequest) error { return nil }), resulttest.Gate(func(context.Context, safety.PublicationRequest) (safety.PublicationDecision, error) {
			return safety.PublicationDecision{Text: "public", Status: safety.StatusRedacted}, nil
		}))...)
		eng, err := engine.New(opts...)
		require.NoError(t, err)
		api := New(eng, nil)
		writer := &stalledReplayWriter{deadlineRecorder: &deadlineRecorder{ResponseRecorder: httptest.NewRecorder()}}
		started := time.Now()
		api.Handler().ServeHTTP(writer, httptest.NewRequestWithContext(ctx, http.MethodGet, "/v1/runs/"+receipt.Request.Current.RunID.String()+"/results/"+receipt.Request.ResultID.String(), nil))
		require.Equal(t, contentWriteTimeout, time.Since(started))
		require.Zero(t, api.contentReads.Load())
		require.Empty(t, writer.Body.String())
	})
}

func TestResultContentCapacityReleasesCancelledReads(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		st := contentStore(t)
		var authorized atomic.Int64
		opts := append([]engine.Option{engine.WithStore(st)}, resulttest.Options(resulttest.Authorizer(func(ctx context.Context, _ verification.ResultContentRequest) error {
			authorized.Add(1)
			<-ctx.Done()
			return ctx.Err()
		}), resulttest.Gate(func(context.Context, safety.PublicationRequest) (safety.PublicationDecision, error) {
			panic("screening must not run")
		}))...)
		eng, err := engine.New(opts...)
		require.NoError(t, err)
		api := New(eng, nil)
		handler := api.Handler()
		ctx, cancel := context.WithCancel(cortex.WithScope(context.Background(), cortex.Scope{Levels: []cortex.Level{{Key: "tenant", Value: "one"}}}))
		defer cancel()
		path := "/v1/runs/" + id.NewAgentRunID().String() + "/results/" + id.New(verification.PrefixResult).String()
		done := make(chan struct{}, maxContentReads)
		for range maxContentReads {
			go func() {
				defer func() { done <- struct{}{} }()
				handler.ServeHTTP(&deadlineRecorder{ResponseRecorder: httptest.NewRecorder()}, httptest.NewRequestWithContext(ctx, http.MethodGet, path, nil))
			}()
		}
		synctest.Wait()
		require.Equal(t, int64(maxContentReads), api.contentReads.Load())
		require.Equal(t, int64(maxContentReads), authorized.Load())
		overflow := httptest.NewRecorder()
		handler.ServeHTTP(overflow, httptest.NewRequestWithContext(ctx, http.MethodGet, path, nil))
		require.Equal(t, 503, overflow.Code)
		require.Contains(t, overflow.Body.String(), "result_busy")
		cancel()
		for range maxContentReads {
			<-done
		}
		require.Zero(t, api.contentReads.Load())
	})
}

func TestResultContentLiveHTTPReauthorizes(t *testing.T) {
	st := contentStore(t)
	ctx := cortex.WithScope(context.Background(), cortex.Scope{Levels: []cortex.Level{{Key: "tenant", Value: "one"}}})
	receipt := resulttest.Commit(ctx, t, st)
	var revoked atomic.Bool
	opts := append([]engine.Option{engine.WithStore(st)}, resulttest.Options(resulttest.Authorizer(func(context.Context, verification.ResultContentRequest) error {
		if revoked.Load() {
			return errors.New("SECRET grant")
		}
		return nil
	}), resulttest.Gate(func(context.Context, safety.PublicationRequest) (safety.PublicationDecision, error) {
		return safety.PublicationDecision{Text: "public projection", Status: safety.StatusRedacted}, nil
	}))...)
	eng, err := engine.New(opts...)
	require.NoError(t, err)
	handler := New(eng, nil).Handler()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		handler.ServeHTTP(w, r.WithContext(cortex.WithScope(r.Context(), cortex.ScopeFromContext(ctx))))
	}))
	defer server.Close()
	url := server.URL + "/v1/runs/" + receipt.Request.Current.RunID.String() + "/results/" + receipt.Request.ResultID.String()
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	require.NoError(t, err)
	response, err := server.Client().Do(request)
	require.NoError(t, err)
	require.Equal(t, 200, response.StatusCode)
	require.Equal(t, "no-store", response.Header.Get("Cache-Control"))
	require.Positive(t, response.ContentLength)
	var result verification.ResultContentResponse
	require.NoError(t, json.NewDecoder(response.Body).Decode(&result))
	require.NoError(t, response.Body.Close())
	require.Equal(t, "public projection", result.Content)
	revoked.Store(true)
	request, err = http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	require.NoError(t, err)
	response, err = server.Client().Do(request)
	require.NoError(t, err)
	require.Equal(t, 403, response.StatusCode)
	require.NoError(t, response.Body.Close())
}

func TestResultContentOpenAPIRevisionIsString(t *testing.T) {
	eng, err := engine.New()
	require.NoError(t, err)
	router := forge.NewRouter(forge.WithOpenAPI(forge.OpenAPIConfig{Title: "test", Version: "1"}))
	require.NoError(t, New(eng, router).RegisterRoutes(router))
	spec := router.OpenAPISpec()
	require.NotNil(t, spec)
	found := false
	for _, schema := range spec.Components.Schemas {
		if field, ok := schema.Properties["run_revision"]; ok && schema.Properties["content_changed"] != nil {
			require.Equal(t, "string", field.Type)
			found = true
		}
	}
	require.True(t, found, "content response schema missing")
}

func TestHistoricalRunRoutesWithholdCommittedContent(t *testing.T) {
	st := contentStore(t)
	ctx := cortex.WithScope(t.Context(), cortex.Scope{Levels: []cortex.Level{{Key: "tenant", Value: "one"}}})
	receipt := resulttest.Commit(ctx, t, st)
	eng, err := engine.New(engine.WithStore(st))
	require.NoError(t, err)
	for _, path := range []string{"/v1/runs?limit=10&offset=0", "/v1/runs/" + receipt.Request.Current.RunID.String()} {
		recorder := httptest.NewRecorder()
		New(eng, nil).Handler().ServeHTTP(recorder, httptest.NewRequestWithContext(ctx, http.MethodGet, path, nil))
		require.Equal(t, 200, recorder.Code, recorder.Body.String())
		require.Equal(t, "no-store", recorder.Header().Get("Cache-Control"))
		require.NotContains(t, recorder.Body.String(), "SECRET")
		require.Contains(t, recorder.Body.String(), receipt.Request.ResultID.String())
	}
	saved, err := st.GetRun(ctx, receipt.Request.Current.RunID)
	require.NoError(t, err)
	require.Equal(t, "SECRET candidate", saved.Output)
}
