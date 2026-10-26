//go:build linux || darwin

package main

import (
	"context"
	"errors"
	"testing"

	"github.com/xraph/cortex/integrations/sandbox/internal/service"
)

func TestCommandOwnsVerifierAndDaemonLifetime(t *testing.T) {
	for _, mode := range []string{"success", "load-error", "missing-verifier", "daemon-error", "cancel"} {
		t.Run(mode, func(t *testing.T) {
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			loaded, ran, closed := 0, 0, 0
			ports := commandPorts{
				load: func(_ context.Context, name string) (qualificationSource, error) {
					loaded++
					if name != "/etc/cortex/qualification.json" {
						t.Fatal("wrong authority config")
					}
					source := qualificationSource{verify: func(context.Context, service.DeploymentIdentity) (service.DeploymentQualification, error) {
						return service.DeploymentQualification{}, nil
					}, close: func() { closed++ }}
					if mode == "load-error" {
						return source, errors.New("secret configuration")
					}
					if mode == "missing-verifier" {
						source.verify = nil
					}
					return source, nil
				},
				run: func(operation context.Context, name string, verify service.QualificationVerifier) error {
					ran++
					if name != "/etc/cortex/broker.json" || verify == nil || closed != 0 {
						t.Fatal("lifetime or binding lost")
					}
					if mode == "daemon-error" {
						return errors.New("private diagnostic")
					}
					if mode == "cancel" {
						cancel()
						return operation.Err()
					}
					return nil
				},
			}
			code := runCommand(ctx, []string{"--config", "/etc/cortex/broker.json", "--qualification-config", "/etc/cortex/qualification.json"}, ports)
			want := 0
			if mode != "success" {
				want = 1
			}
			if code != want || loaded != 1 || closed != 1 {
				t.Fatalf("code=%d loads=%d closes=%d", code, loaded, closed)
			}
			expectedRuns := 1
			if mode == "load-error" || mode == "missing-verifier" {
				expectedRuns = 0
			}
			if ran != expectedRuns {
				t.Fatal("unexpected daemon calls", ran)
			}
		})
	}
}

func TestCommandRejectsAmbiguousArgumentsBeforeServices(t *testing.T) {
	valid := []string{"--config", "/etc/cortex/broker.json", "--qualification-config", "/etc/cortex/qualification.json"}
	tests := [][]string{nil, append(append([]string{}, valid...), "extra"), {"--config", "relative", "--qualification-config", valid[3]}, {"--config", "/etc/../broker", "--qualification-config", valid[3]}, {"--config", "/", "--qualification-config", valid[3]}, {"--config", valid[1], "--config", valid[3]}, {"--config", valid[1], "--qualification-config", ""}, {"--config", valid[1], "--qualification-config", valid[1]}, {"--config", valid[1], "--qualification-config", "/etc/secret\x00"}, {"--config", valid[1], "--qualified", "true"}}
	for _, args := range tests {
		ports := commandPorts{load: func(context.Context, string) (qualificationSource, error) {
			t.Fatal("invalid args opened authority")
			return qualificationSource{}, nil
		}, run: func(context.Context, string, service.QualificationVerifier) error {
			t.Fatal("invalid args opened daemon")
			return nil
		}}
		if runCommand(t.Context(), args, ports) == 0 {
			t.Fatal("invalid arguments accepted")
		}
	}
	if runCommand(t.Context(), valid, commandPorts{}) == 0 {
		t.Fatal("missing services accepted")
	}
}

func TestCommandCancellationReachesDaemon(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	entered, done := make(chan struct{}), make(chan int, 1)
	closed := false
	ports := commandPorts{load: func(context.Context, string) (qualificationSource, error) {
		return qualificationSource{verify: func(context.Context, service.DeploymentIdentity) (service.DeploymentQualification, error) {
			return service.DeploymentQualification{}, nil
		}, close: func() { closed = true }}, nil
	}, run: func(operation context.Context, _ string, _ service.QualificationVerifier) error {
		close(entered)
		<-operation.Done()
		return operation.Err()
	}}
	go func() {
		done <- runCommand(ctx, []string{"--config", "/etc/broker", "--qualification-config", "/etc/authority"}, ports)
	}()
	<-entered
	cancel()
	if code := <-done; code == 0 || !closed {
		t.Fatal("cancelled daemon retained resources")
	}
	ports.load = func(context.Context, string) (qualificationSource, error) {
		t.Fatal("cancelled startup opened authority")
		return qualificationSource{}, nil
	}
	if runCommand(ctx, []string{"--config", "/etc/broker", "--qualification-config", "/etc/authority"}, ports) == 0 {
		t.Fatal("cancelled startup succeeded")
	}
}
