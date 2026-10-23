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
	code := runCommand(ctx, os.Args[1:], service.ProvisionInput, os.Stdout)
	stop()
	if code != 0 {
		log.Print("cortex: input provisioning failed; check arguments, protected files and current grants")
	}
	os.Exit(code)
}
