package cortex_test

import (
	"context"
	"errors"
	"testing"

	"github.com/xraph/cortex"
)

type testPrincipal struct {
	User string `json:"user"`
	Org  string `json:"org"`
}

// The codec hands back a value of the type it was built for, so an
// authorizer's type assertion on a resumed run's principal holds exactly
// as it does on a fresh one.
func TestJSONPrincipalCodecRoundTrip(t *testing.T) {
	ctx := context.Background()
	codec := cortex.JSONPrincipalCodec[testPrincipal]()
	want := testPrincipal{User: "alice", Org: "o1"}

	data, err := codec.EncodePrincipal(ctx, want)
	if err != nil {
		t.Fatalf("EncodePrincipal: %v", err)
	}
	decoded, err := codec.DecodePrincipal(ctx, data)
	if err != nil {
		t.Fatalf("DecodePrincipal: %v", err)
	}
	got, ok := decoded.(testPrincipal)
	if !ok {
		t.Fatalf("DecodePrincipal returned %T, want testPrincipal", decoded)
	}
	if got != want {
		t.Errorf("round trip = %+v, want %+v", got, want)
	}
}

// A principal of another type is refused at encode time. Accepting it
// would persist bytes that decode into a T the host never put on the
// context, and the authorizer would meet a stranger after the pause.
func TestJSONPrincipalCodecRefusesOtherTypes(t *testing.T) {
	codec := cortex.JSONPrincipalCodec[testPrincipal]()
	if _, err := codec.EncodePrincipal(context.Background(), "alice"); err == nil {
		t.Fatal("EncodePrincipal accepted a string for a testPrincipal codec")
	}
	if _, err := codec.DecodePrincipal(context.Background(), []byte("{")); err == nil || errors.Is(err, cortex.ErrPrincipalRevoked) {
		t.Fatalf("DecodePrincipal on malformed bytes = %v, want a transient error", err)
	}
}
