//go:build linux || darwin

package main

import (
	"context"
	"path/filepath"
	"strings"
	"time"

	"github.com/xraph/cortex/integrations/sandbox/internal/service"
)

type qualificationSource struct {
	verify service.QualificationVerifier
	close  func()
}

type commandPorts struct {
	load func(context.Context, string) (qualificationSource, error)
	run  func(context.Context, string, service.QualificationVerifier) error
}

// runCommand keeps diagnostics inside the process. Both configuration paths
// must be explicit; no argument can supply qualification or disable its checks.
func runCommand(ctx context.Context, args []string, ports commandPorts) int {
	if ctx.Err() != nil || ports.load == nil || ports.run == nil || len(args) != 4 || args[0] != "--config" || args[2] != "--qualification-config" || !configPath(args[1]) || !configPath(args[3]) || args[1] == args[3] {
		return 1
	}
	preparation, cancel := context.WithTimeout(ctx, 30*time.Second)
	source, err := ports.load(preparation, args[3])
	preparationErr := preparation.Err()
	cancel()
	if source.close != nil {
		defer source.close()
	}
	if err != nil || preparationErr != nil || source.verify == nil || source.close == nil || ctx.Err() != nil {
		return 1
	}
	if err = ports.run(ctx, args[1], source.verify); err != nil || ctx.Err() != nil {
		return 1
	}
	return 0
}

func configPath(name string) bool {
	return name != "/" && len(name) <= 1024 && filepath.IsAbs(name) && filepath.Clean(name) == name && !strings.ContainsRune(name, 0)
}
