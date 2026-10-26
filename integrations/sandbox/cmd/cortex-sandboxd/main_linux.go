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
	code := runCommand(ctx, os.Args[1:], commandPorts{
		load: func(operation context.Context, name string) (qualificationSource, error) {
			client, err := service.OpenQualificationClient(operation, name)
			if err != nil {
				return qualificationSource{}, err
			}
			return qualificationSource{verify: client.Verify, close: client.Close}, nil
		},
		run: service.RunInstalledDaemon,
	})
	stop()
	if code != 0 {
		log.Print("cortex: sandbox broker stopped; check protected configuration, services and qualification authority")
	}
	os.Exit(code)
}
