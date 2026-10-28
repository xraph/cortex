package cortex

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
)

// PrincipalCodec carries a run's principal across a pause. A principal
// lives on the context, and a context does not survive a suspension: the
// run is stored, and whoever resumes it brings a context of their own. So
// the engine encodes the principal when it creates a run, stores the
// bytes with it, and decodes them on every resume, putting the original
// caller back where the resumer's would otherwise be.
//
// The bytes are the host's own. Cortex stores them and hands them back,
// and never reads them. A codec can write the whole identity, or only a
// reference it looks up again on decode, which is also the moment to find
// out the identity is gone.
type PrincipalCodec interface {
	EncodePrincipal(ctx context.Context, principal any) ([]byte, error)
	// DecodePrincipal restores a run's principal. Return an error wrapping ErrPrincipalRevoked when
	// the identity is permanently gone; any other error is treated as transient.
	DecodePrincipal(ctx context.Context, data []byte) (any, error)
}

// JSONPrincipalCodec encodes a principal of type T with json.Marshal and
// decodes it with json.Unmarshal into a T value, so an authorizer that
// asserts principal.(T) on a fresh run sees the same type on a resumed
// one. A principal of any other type is refused rather than written,
// because it would come back as a T the host never put on the context.
//
// Stored bytes that no longer decode into a T, because they are not JSON
// or because T changed shape since the run was created, are reported as
// ErrPrincipalRevoked: retrying cannot change them, so the run is failed
// rather than left paused. Any other decode error stays transient.
//
// It has no other way to tell a revoked identity from a live one. A host
// that needs that check writes its own codec, or wraps this one.
func JSONPrincipalCodec[T any]() PrincipalCodec {
	return jsonPrincipalCodec[T]{}
}

type jsonPrincipalCodec[T any] struct{}

func (jsonPrincipalCodec[T]) EncodePrincipal(_ context.Context, principal any) ([]byte, error) {
	typed, ok := principal.(T)
	if !ok {
		var want T
		return nil, fmt.Errorf("cortex: principal is %T, the codec encodes %T", principal, want)
	}
	data, err := json.Marshal(typed)
	if err != nil {
		return nil, fmt.Errorf("cortex: encode principal: %w", err)
	}
	return data, nil
}

func (jsonPrincipalCodec[T]) DecodePrincipal(_ context.Context, data []byte) (any, error) {
	var principal T
	if err := json.Unmarshal(data, &principal); err != nil {
		var syntaxErr *json.SyntaxError
		var typeErr *json.UnmarshalTypeError
		if errors.As(err, &syntaxErr) || errors.As(err, &typeErr) {
			return nil, fmt.Errorf("cortex: stored principal is unreadable: %w: %w", ErrPrincipalRevoked, err)
		}
		return nil, fmt.Errorf("cortex: decode principal: %w", err)
	}
	return principal, nil
}
