package main

import (
	"context"
	"os"
	"os/signal"
	"syscall"

	"github.com/xraph/cortex/integrations/sandbox/internal/service"
	"github.com/xraph/cortex/integrations/sandbox/internal/worker"
)

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	code := runCommand(ctx, os.Args[1:], os.Getenv("CREDENTIALS_DIRECTORY"), commandPorts{bootstrap: service.BootstrapWorker, execute: worker.Run})
	stop()
	os.Exit(code)
}
