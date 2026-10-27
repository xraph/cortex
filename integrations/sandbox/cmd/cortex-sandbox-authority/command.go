//go:build linux || darwin

package main

import (
	"context"
	"path/filepath"
	"strings"
)

func runCommand(ctx context.Context, args []string, run func(context.Context, string) error) int {
	if ctx.Err() != nil || run == nil || len(args) != 2 || args[0] != "--config" {
		return 1
	}
	name := args[1]
	if len(name) > 1024 || name == "/" || !filepath.IsAbs(name) || filepath.Clean(name) != name || strings.ContainsRune(name, 0) {
		return 1
	}
	if run(ctx, name) != nil || ctx.Err() != nil {
		return 1
	}
	return 0
}
