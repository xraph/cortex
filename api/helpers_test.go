package api

import (
	"fmt"
	"net/http"
	"strings"
	"testing"

	"github.com/xraph/cortex"
	"github.com/xraph/cortex/safety"
)

func TestRunRevisionConflictHTTPStatus(t *testing.T) {
	for _, cause := range []error{cortex.ErrRunConflict, fmt.Errorf("private details: %w", cortex.ErrRunConflict)} {
		mapped := mapStoreError(cause)
		status, ok := mapped.(statusCoder)
		if !ok || status.StatusCode() != http.StatusConflict {
			t.Fatalf("revision conflict mapping: %v", mapped)
		}
		if strings.Contains(mapped.Error(), "private details") {
			t.Fatal("conflict disclosed wrapped details")
		}
	}
}

// statusCoder mirrors forge/errs.HTTPError's StatusCode() method without
// importing the concrete type, so this test only depends on the
// package-level forge.NewHTTPError/NotFound contract already used by
// mapStoreError.
type statusCoder interface {
	StatusCode() int
}

func TestMapStoreError_NoScopeIsPreconditionFailed(t *testing.T) {
	err := mapStoreError(cortex.ErrNoScope)
	sc, ok := err.(statusCoder)
	if !ok {
		t.Fatalf("mapStoreError(ErrNoScope) = %T, want an error exposing StatusCode()", err)
	}
	if sc.StatusCode() != http.StatusPreconditionFailed {
		t.Fatalf("status = %d, want %d", sc.StatusCode(), http.StatusPreconditionFailed)
	}
}

func TestMapStoreError_NotFoundStillMapsToNotFound(t *testing.T) {
	err := mapStoreError(cortex.ErrRunNotFound)
	sc, ok := err.(statusCoder)
	if !ok {
		t.Fatalf("mapStoreError(ErrRunNotFound) = %T, want an error exposing StatusCode()", err)
	}
	if sc.StatusCode() != http.StatusNotFound {
		t.Fatalf("status = %d, want %d", sc.StatusCode(), http.StatusNotFound)
	}
}

func TestMapStoreError_NilIsNil(t *testing.T) {
	if err := mapStoreError(nil); err != nil {
		t.Fatalf("mapStoreError(nil) = %v, want nil", err)
	}
}

func TestMapStoreError_InvalidScopeIsDenied(t *testing.T) {
	for _, cause := range []error{cortex.ErrInvalidScope, cortex.ErrUnsupportedScopeDepth} {
		mapped := mapStoreError(cause)
		status, ok := mapped.(statusCoder)
		if !ok || status.StatusCode() != http.StatusForbidden {
			t.Fatalf("invalid scope mapping: %v", mapped)
		}
	}
}

func TestPublicationErrorsHaveSafeHTTPStatus(t *testing.T) {
	for cause, want := range map[error]int{cortex.ErrPreparationUnavailable: 503, safety.ErrUnavailable: 503, safety.ErrBlocked: 403, safety.ErrLimit: 413} {
		mapped, ok := mapStoreError(cause).(statusCoder)
		if !ok || mapped.StatusCode() != want {
			t.Fatalf("%v mapped to %v", cause, mapped)
		}
	}
}

// A replica that is stopping answers 503, so a client retries elsewhere,
// and a run that was handed off answers 409: it is waiting to be
// continued, not failed. Neither discloses what it wraps.
func TestMapStoreError_LifecycleErrors(t *testing.T) {
	for _, tc := range []struct {
		err  error
		want int
	}{
		{cortex.ErrEngineStopping, http.StatusServiceUnavailable},
		{fmt.Errorf("private details: %w", cortex.ErrEngineStopping), http.StatusServiceUnavailable},
		{cortex.ErrRunInterrupted, http.StatusConflict},
		{fmt.Errorf("private details: %w", cortex.ErrRunInterrupted), http.StatusConflict},
		// A stop that interrupted a run is about the replica first.
		{fmt.Errorf("private details: %w: %w", cortex.ErrRunInterrupted, cortex.ErrEngineStopping), http.StatusServiceUnavailable},
	} {
		mapped := mapStoreError(tc.err)
		status, ok := mapped.(statusCoder)
		if !ok || status.StatusCode() != tc.want {
			t.Errorf("mapStoreError(%v) = %v, want status %d", tc.err, mapped, tc.want)
			continue
		}
		if strings.Contains(mapped.Error(), "private details") {
			t.Errorf("mapStoreError(%v) disclosed wrapped details: %v", tc.err, mapped)
		}
	}
}
