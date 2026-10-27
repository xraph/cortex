package main

import (
	"context"
	"log"
	"os"
	"os/signal"
	"syscall"

	"github.com/xraph/cortex/integrations/sandbox/internal/service"
)

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	code := runCommand(ctx, os.Args[1:], service.RunQualificationAuthority)
	stop()
	if code != 0 {
		log.Print("cortex: sandbox evidence authority stopped; check protected configuration and retained evidence")
	}
	os.Exit(code)
}
