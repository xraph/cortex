package sandboxbroker

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"

	"github.com/xraph/cortex/id"
	"github.com/xraph/cortex/sandbox"
)

// Authenticator validates transport credentials and returns the host-mapped
// identity. It must not derive grants from request JSON or unsigned headers.
type Authenticator func(*http.Request) (Identity, error)

// Handler requires TLS and authenticates each request before reading its body.
// You can use client certificates or host-issued credentials in Authenticate.
func (b *Broker) Handler(authenticate Authenticator) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		if authenticate == nil || r.TLS == nil {
			http.Error(w, "access denied", http.StatusUnauthorized)
			return
		}
		who, err := authenticate(r)
		if err != nil {
			http.Error(w, "access denied", http.StatusUnauthorized)
			return
		}
		ctx := WithIdentity(r.Context(), who)
		var value any
		switch {
		case r.URL.Path == "/v1/capabilities" && r.Method == http.MethodGet:
			value, err = b.Capabilities(ctx)
		case r.URL.Path == "/v1/jobs" && r.Method == http.MethodPost:
			var j sandbox.Job
			r.Body = http.MaxBytesReader(w, r.Body, 1<<20)
			decoder := json.NewDecoder(r.Body)
			decoder.DisallowUnknownFields()
			err = decoder.Decode(&j)
			if err == nil {
				var extra any
				if nextErr := decoder.Decode(&extra); !errors.Is(nextErr, io.EOF) {
					err = ErrDenied
				}
			}
			if err != nil {
				http.Error(w, "invalid job", http.StatusBadRequest)
				return
			}
			value, err = b.Submit(ctx, j)
		default:
			tail, ok := strings.CutPrefix(r.URL.Path, "/v1/jobs/")
			if !ok {
				http.NotFound(w, r)
				return
			}
			keyText, operation, _ := strings.Cut(tail, "/")
			var key id.ID
			key, err = id.Parse(keyText)
			if err != nil {
				http.NotFound(w, r)
				return
			}
			switch {
			case operation == "" && r.Method == http.MethodGet:
				value, err = b.Inspect(ctx, key)
			case operation == "result" && r.Method == http.MethodGet:
				value, err = b.Collect(ctx, key)
			case operation == "cancel" && r.Method == http.MethodPost:
				err = b.Cancel(ctx, key)
				value = struct{}{}
			default:
				http.NotFound(w, r)
				return
			}
		}
		if err != nil {
			status := http.StatusServiceUnavailable
			switch {
			case err == ErrNotFound: //nolint:errorlint // Wrapped or joined failures do not confirm absence.
				status = http.StatusNotFound
			case errors.Is(err, ErrDenied):
				status = http.StatusForbidden
			case errors.Is(err, ErrConflict):
				status = http.StatusConflict
			case err == ErrPending: //nolint:errorlint // A joined runtime failure must remain unavailable.
				status = http.StatusAccepted
			}
			// Do not publish runtime paths, credentials or backend errors.
			http.Error(w, http.StatusText(status), status)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		if encodeErr := json.NewEncoder(w).Encode(value); encodeErr != nil {
			return
		}
	})
}
