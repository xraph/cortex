package engine

import (
	"context"
	"errors"
	"testing"

	"github.com/xraph/cortex"
)

func TestResolveCloneName(t *testing.T) {
	ctx := context.Background()

	// Desired name, free → returned as-is.
	got, err := resolveCloneName(ctx, "want", "src", func(context.Context, string) (bool, error) {
		return false, nil
	})
	if err != nil || got != "want" {
		t.Fatalf("desired-free: got %q, %v; want want,nil", got, err)
	}

	// Desired name, taken → ErrAlreadyExists.
	_, err = resolveCloneName(ctx, "want", "src", func(context.Context, string) (bool, error) {
		return true, nil
	})
	if !errors.Is(err, cortex.ErrAlreadyExists) {
		t.Fatalf("desired-taken: err = %v, want ErrAlreadyExists", err)
	}

	// Empty desired, "src-copy" taken → "src-copy-2".
	taken := map[string]bool{"src-copy": true}
	got, err = resolveCloneName(ctx, "", "src", func(_ context.Context, n string) (bool, error) {
		return taken[n], nil
	})
	if err != nil || got != "src-copy-2" {
		t.Fatalf("auto-fallback: got %q, %v; want src-copy-2,nil", got, err)
	}

	// Empty desired, all free → "src-copy".
	got, err = resolveCloneName(ctx, "", "src", func(context.Context, string) (bool, error) {
		return false, nil
	})
	if err != nil || got != "src-copy" {
		t.Fatalf("auto-first: got %q, %v; want src-copy,nil", got, err)
	}
}

func TestCloneNoStore(t *testing.T) {
	e, err := New()
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	ctx := context.Background()
	if _, err := e.CloneAgent(ctx, "app1", "x", ""); !errors.Is(err, cortex.ErrNoStore) {
		t.Errorf("CloneAgent err = %v, want ErrNoStore", err)
	}
	if _, err := e.ClonePersona(ctx, "app1", "x", ""); !errors.Is(err, cortex.ErrNoStore) {
		t.Errorf("ClonePersona err = %v, want ErrNoStore", err)
	}
}
