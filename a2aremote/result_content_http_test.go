package a2aremote

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"testing/synctest"
	"time"

	"github.com/xraph/cortex/verification"
)

func contentHTTPRequest(ctx context.Context, binding string, cred Credentials, req GetTaskRequest) *http.Request {
	var r *http.Request
	if binding == "rest" {
		r = httptest.NewRequestWithContext(ctx, http.MethodGet, "/worker/tasks/"+req.ID, nil)
	} else {
		r = httptest.NewRequestWithContext(ctx, http.MethodPost, "/", strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"GetTask","params":{"tenant":"worker","id":"`+req.ID+`"}}`))
	}
	r.Header = http.Header(cred.Headers).Clone()
	return r
}

type contentHTTPWriter struct {
	*httptest.ResponseRecorder
	deadline time.Time
	stall    bool
	writes   int
}

func (w *contentHTTPWriter) SetWriteDeadline(d time.Time) error { w.deadline = d; return nil }
func (w *contentHTTPWriter) Write(b []byte) (int, error) {
	w.writes++
	if w.stall {
		time.Sleep(time.Until(w.deadline))
		return 0, context.DeadlineExceeded
	}
	return w.ResponseRecorder.Write(b)
}

func TestRemoteContentHTTPBindings(t *testing.T) {
	for _, binding := range []string{"rest", "rpc"} {
		t.Run(binding, func(t *testing.T) {
			svc, gw, cred, req := contentFixture(t)
			handler := svc.RESTHandler()
			if binding == "rpc" {
				handler = svc.JSONRPCHandler()
			}
			recorder := &contentHTTPWriter{ResponseRecorder: httptest.NewRecorder()}
			handler.ServeHTTP(recorder, contentHTTPRequest(t.Context(), binding, cred, req))
			if recorder.Code != 200 || recorder.Header().Get("A2A-Extensions") != ResultContentExtensionURI || recorder.Header().Get("Cache-Control") != "no-store" || !strings.Contains(recorder.Body.String(), `"content":"screened"`) || strings.Contains(recorder.Body.String(), "SECRET") {
				t.Fatalf("bad content response: %d %s", recorder.Code, recorder.Body.String())
			}
			gw.read = func(context.Context, verification.ResultContentRequest) (verification.PublishedResult, error) {
				return verification.PublishedResult{}, verification.ErrResultAccessDenied
			}
			recorder = &contentHTTPWriter{ResponseRecorder: httptest.NewRecorder()}
			handler.ServeHTTP(recorder, contentHTTPRequest(t.Context(), binding, cred, req))
			if !strings.Contains(recorder.Body.String(), "result_denied") || strings.Contains(recorder.Body.String(), "screened") || recorder.Header().Get("Cache-Control") != "no-store" || recorder.Header().Get("A2A-Extensions") != "" {
				t.Fatalf("bad denial: %s", recorder.Body.String())
			}
			gw.read = func(context.Context, verification.ResultContentRequest) (verification.PublishedResult, error) {
				t.Fatal("unsupported writer reached private content")
				return verification.PublishedResult{}, nil
			}
			unsupported := httptest.NewRecorder()
			handler.ServeHTTP(unsupported, contentHTTPRequest(t.Context(), binding, cred, req))
			if !strings.Contains(unsupported.Body.String(), "result_unavailable") || svc.contentDeliveries.Load() != 0 {
				t.Fatal("unsupported write deadline accepted")
			}
		})
	}
}

func TestRemoteContentHTTPStalledWrite(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		svc, _, cred, req := contentFixture(t)
		writer := &contentHTTPWriter{ResponseRecorder: httptest.NewRecorder(), stall: true}
		start := time.Now()
		svc.RESTHandler().ServeHTTP(writer, contentHTTPRequest(t.Context(), "rest", cred, req))
		if time.Since(start) != 10*time.Second || writer.writes != 1 || svc.contentDeliveries.Load() != 0 || svc.contentReads.Load() != 0 {
			t.Fatal("stalled delivery escaped bound or leaked admission")
		}
	})
}

func TestRemoteContentAdmissionCancellation(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		svc, gw, cred, req := contentFixture(t)
		gw.read = func(ctx context.Context, _ verification.ResultContentRequest) (verification.PublishedResult, error) {
			<-ctx.Done()
			return verification.PublishedResult{}, ctx.Err()
		}
		ctx, cancel := context.WithCancel(t.Context())
		var done sync.WaitGroup
		for range 64 {
			done.Go(func() {
				svc.RESTHandler().ServeHTTP(&contentHTTPWriter{ResponseRecorder: httptest.NewRecorder()}, contentHTTPRequest(ctx, "rest", cred, req))
			})
		}
		synctest.Wait()
		if svc.contentReads.Load() != 64 || svc.contentDeliveries.Load() != 64 {
			t.Fatal("admission was not filled")
		}
		recorder := &contentHTTPWriter{ResponseRecorder: httptest.NewRecorder()}
		svc.RESTHandler().ServeHTTP(recorder, contentHTTPRequest(t.Context(), "rest", cred, req))
		if !strings.Contains(recorder.Body.String(), "result_unavailable") {
			t.Fatal("overflow accepted")
		}
		if task, err := svc.GetTask(t.Context(), cred, req); err == nil || task != nil {
			t.Fatal("direct reader admission overflow accepted")
		}
		cancel()
		done.Wait()
		if svc.contentReads.Load() != 0 || svc.contentDeliveries.Load() != 0 {
			t.Fatal("cancelled slots leaked")
		}
	})
}

func TestRemoteContentLiveHTTP(t *testing.T) {
	svc, _, cred, req := contentFixture(t)
	server := httptest.NewServer(svc.RESTHandler())
	defer server.Close()
	request, err := http.NewRequestWithContext(t.Context(), http.MethodGet, server.URL+"/worker/tasks/"+req.ID, http.NoBody)
	if err != nil {
		t.Fatal(err)
	}
	request.Header = http.Header(cred.Headers).Clone()
	response, err := server.Client().Do(request)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	body, err := io.ReadAll(response.Body)
	if err != nil || response.StatusCode != 200 || !strings.Contains(string(body), `"content":"screened"`) || response.Header.Get("Cache-Control") != "no-store" {
		t.Fatalf("live HTTP: %s %v", body, err)
	}
}

func TestRemoteContentNotificationHasNoErrorBody(t *testing.T) {
	svc, _, cred, req := contentFixture(t)
	request := httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/", strings.NewReader(`{"jsonrpc":"2.0","method":"GetTask","params":{"tenant":"worker","id":"`+req.ID+`"}}`))
	request.Header = http.Header(cred.Headers).Clone()
	recorder := httptest.NewRecorder() // No deadline support, so preparation fails.
	svc.JSONRPCHandler().ServeHTTP(recorder, request)
	if recorder.Code != http.StatusNoContent || recorder.Body.Len() != 0 {
		t.Fatalf("notification received response: %d %s", recorder.Code, recorder.Body.String())
	}
}
