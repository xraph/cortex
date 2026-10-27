//go:build linux || darwin

package main

import (
	"context"
	"errors"
	"testing"
)

func TestAuthorityCommandRequiresExplicitProtectedConfig(t *testing.T) {
	for _, args := range [][]string{nil, {"--config"}, {"--config", "relative"}, {"--config", "/etc/../policy"}, {"--config", "/etc/policy", "--fixture"}, {"--config", "/etc/policy"}} {
		calls := 0
		result := runCommand(t.Context(), args, func(_ context.Context, name string) error {
			calls++
			if name != "/etc/policy" {
				t.Fatal(name)
			}
			return nil
		})
		valid := len(args) == 2 && args[1] == "/etc/policy"
		if valid && (result != 0 || calls != 1) {
			t.Fatal("valid command refused")
		}
		if !valid && (result != 1 || calls != 0) {
			t.Fatal("invalid command executed")
		}
	}
}
func TestAuthorityCommandPropagatesFailureAndCancellation(t *testing.T) {
	args := []string{"--config", "/etc/policy"}
	if runCommand(t.Context(), args, func(context.Context, string) error { return errors.New("private detail") }) != 1 {
		t.Fatal("failure ignored")
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if runCommand(ctx, args, func(context.Context, string) error { t.Fatal("cancelled command executed"); return nil }) != 1 {
		t.Fatal("cancellation ignored")
	}
	if runCommand(t.Context(), args, nil) != 1 {
		t.Fatal("missing runner accepted")
	}
}
