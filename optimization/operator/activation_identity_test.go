package operator

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/xraph/cortex"
	opt "github.com/xraph/cortex/optimization"
)

func TestActivationCommandIdentityScopeAndKey(t *testing.T) {
	scope := cortex.Scope{Levels: []cortex.Level{{Key: "org", Value: "a"}, {Key: "project", Value: "b"}}}
	ctx := cortex.WithScope(context.Background(), scope)
	first, err := activationCommandID(ctx, "command-key")
	if err != nil {
		t.Fatal(err)
	}
	second, err := activationCommandID(ctx, "command-key")
	if err != nil || second != first {
		t.Fatalf("unstable identity: %v", err)
	}
	if first.Prefix() != opt.PrefixActivation {
		t.Fatal("wrong activation identity")
	}
	different, err := activationCommandID(ctx, "other-key")
	if err != nil || first == different {
		t.Fatal("different keys share identity")
	}
	scope.Levels[1].Value = "other"
	foreign, err := activationCommandID(cortex.WithScope(context.Background(), scope), "command-key")
	if err != nil || first == foreign {
		t.Fatal("different scopes share identity")
	}
	for _, key := range []string{"", " padded", "bad\nkey", strings.Repeat("x", 513)} {
		if _, err = activationCommandID(ctx, key); !errors.Is(err, opt.ErrInvalidSpec) {
			t.Fatalf("invalid key: %v", err)
		}
	}
	if _, err = activationCommandID(context.Background(), "command-key"); !errors.Is(err, cortex.ErrNoScope) {
		t.Fatalf("missing scope: %v", err)
	}
}
