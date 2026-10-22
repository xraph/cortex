//go:build linux || darwin

package main

import (
	"context"
	"path/filepath"
	"strings"

	"github.com/xraph/cortex/id"
	"github.com/xraph/cortex/integrations/sandbox/internal/service"
	"github.com/xraph/cortex/sandbox"
)

type commandPorts struct {
	bootstrap func(context.Context, string, string, id.ID) (*service.WorkerSession, error)
	execute   func(context.Context, *service.WorkerSession) (service.WorkerReportReceipt, error)
}

// runCommand accepts the exact installed template invocation. Diagnostic errors
// can contain launch capabilities or source data and never reach process output.
func runCommand(ctx context.Context, args []string, credentials string, ports commandPorts) int {
	if ctx.Err() != nil || ports.bootstrap == nil || ports.execute == nil || len(args) != 4 || args[0] != "--job" || args[2] != "--config" || !launchPath(args[3]) || !launchPath(credentials) {
		return 1
	}
	key, err := id.ParseWithPrefix(args[1], sandbox.PrefixJob)
	if err != nil {
		return 1
	}
	session, err := ports.bootstrap(ctx, args[3], credentials, key)
	if err != nil || session == nil || ctx.Err() != nil {
		return 1
	}
	if _, err = ports.execute(ctx, session); err != nil || ctx.Err() != nil {
		return 1
	}
	return 0
}

func launchPath(name string) bool {
	return name != "/" && len(name) <= 1024 && filepath.IsAbs(name) && filepath.Clean(name) == name && !strings.ContainsRune(name, '\x00')
}
