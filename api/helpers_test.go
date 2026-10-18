package api

import (
	"net/http"
	"testing"

	"github.com/xraph/cortex"
	"github.com/xraph/cortex/safety"
)

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
	for cause, want := range map[error]int{safety.ErrUnavailable: 503, safety.ErrBlocked: 403, safety.ErrLimit: 413} {
		mapped, ok := mapStoreError(cause).(statusCoder)
		if !ok || mapped.StatusCode() != want {
			t.Fatalf("%v mapped to %v", cause, mapped)
		}
	}
}
