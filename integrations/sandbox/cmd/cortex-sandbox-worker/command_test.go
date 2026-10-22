//go:build linux || darwin

package main

import (
	"context"
	"errors"
	"testing"

	"github.com/xraph/cortex/id"
	"github.com/xraph/cortex/integrations/sandbox/internal/service"
	"github.com/xraph/cortex/sandbox"
)

func TestCommandRunsOneAuthenticatedSession(t *testing.T) {
	key := id.New(sandbox.PrefixJob)
	session := &service.WorkerSession{}
	calls := []string{}
	ports := commandPorts{
		bootstrap: func(ctx context.Context, config, credentials string, job id.ID) (*service.WorkerSession, error) {
			calls = append(calls, "bootstrap")
			if ctx.Err() != nil || config != "/etc/cortex/worker.json" || credentials != "/run/credentials/worker" || job != key {
				t.Fatal("launch binding changed")
			}
			return session, nil
		},
		execute: func(_ context.Context, got *service.WorkerSession) (service.WorkerReportReceipt, error) {
			calls = append(calls, "execute")
			if got != session {
				t.Fatal("session was replaced")
			}
			return service.WorkerReportReceipt{JobID: key}, nil
		},
	}
	if code := runCommand(t.Context(), []string{"--job", key.String(), "--config", "/etc/cortex/worker.json"}, "/run/credentials/worker", ports); code != 0 {
		t.Fatal("command failed", code)
	}
	if len(calls) != 2 || calls[0] != "bootstrap" || calls[1] != "execute" {
		t.Fatal("execution order", calls)
	}
}

func TestCommandRejectsAmbiguousLaunchBeforeBootstrap(t *testing.T) {
	key := id.New(sandbox.PrefixJob).String()
	valid := []string{"--job", key, "--config", "/etc/cortex/worker.json"}
	tests := []struct {
		name        string
		args        []string
		credentials string
	}{
		{"empty", nil, "/run/credentials/worker"},
		{"extra", append(append([]string{}, valid...), "/bin/sh"), "/run/credentials/worker"},
		{"duplicate", []string{"--job", key, "--job", key}, "/run/credentials/worker"},
		{"foreign", []string{"--job", id.New(id.PrefixAgent).String(), "--config", "/etc/cortex/worker.json"}, "/run/credentials/worker"},
		{"malformed", []string{"--job", "invalid", "--config", "/etc/cortex/worker.json"}, "/run/credentials/worker"},
		{"relative-config", []string{"--job", key, "--config", "worker.json"}, "/run/credentials/worker"},
		{"config-traversal", []string{"--job", key, "--config", "/etc/../worker.json"}, "/run/credentials/worker"},
		{"missing-credentials", valid, ""},
		{"relative-credentials", valid, "credentials"},
		{"credentials-traversal", valid, "/run/../credentials"},
		{"root-credentials", valid, "/"},
		{"nul", valid, "/run/credentials\x00"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			ports := commandPorts{bootstrap: func(context.Context, string, string, id.ID) (*service.WorkerSession, error) {
				t.Fatal("invalid launch reached bootstrap")
				return nil, nil
			}, execute: func(context.Context, *service.WorkerSession) (service.WorkerReportReceipt, error) {
				t.Fatal("invalid launch executed")
				return service.WorkerReportReceipt{}, nil
			}}
			if code := runCommand(t.Context(), test.args, test.credentials, ports); code == 0 {
				t.Fatal("invalid launch succeeded")
			}
		})
	}
}

func TestCommandDoesNotRetryUncertainExecution(t *testing.T) {
	for _, mode := range []string{"bootstrap", "nil-session", "execute", "cancel-before", "cancel-after-bootstrap"} {
		t.Run(mode, func(t *testing.T) {
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			if mode == "cancel-before" {
				cancel()
			}
			boot, executed := 0, 0
			ports := commandPorts{
				bootstrap: func(context.Context, string, string, id.ID) (*service.WorkerSession, error) {
					boot++
					if mode == "bootstrap" {
						return nil, errors.New("private launch token")
					}
					if mode == "nil-session" {
						return nil, nil
					}
					if mode == "cancel-after-bootstrap" {
						cancel()
					}
					return &service.WorkerSession{}, nil
				},
				execute: func(context.Context, *service.WorkerSession) (service.WorkerReportReceipt, error) {
					executed++
					return service.WorkerReportReceipt{}, errors.New("uncertain report")
				},
			}
			if code := runCommand(ctx, []string{"--job", id.New(sandbox.PrefixJob).String(), "--config", "/etc/cortex/worker.json"}, "/run/credentials/worker", ports); code == 0 {
				t.Fatal("uncertain execution acknowledged")
			}
			wantBoot, wantExecute := 1, 0
			if mode == "cancel-before" {
				wantBoot = 0
			}
			if mode == "execute" {
				wantExecute = 1
			}
			if boot != wantBoot || executed != wantExecute {
				t.Fatal("failed launch retried or executed", boot, executed)
			}
		})
	}
}

func TestCommandCancellationReachesSingleExecution(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	entered := make(chan struct{})
	done := make(chan int, 1)
	key := id.New(sandbox.PrefixJob)
	ports := commandPorts{
		bootstrap: func(context.Context, string, string, id.ID) (*service.WorkerSession, error) {
			return &service.WorkerSession{}, nil
		},
		execute: func(operation context.Context, _ *service.WorkerSession) (service.WorkerReportReceipt, error) {
			close(entered)
			<-operation.Done()
			return service.WorkerReportReceipt{}, operation.Err()
		},
	}
	go func() {
		done <- runCommand(ctx, []string{"--job", key.String(), "--config", "/etc/cortex/worker.json"}, "/run/credentials/worker", ports)
	}()
	<-entered
	cancel()
	if code := <-done; code == 0 {
		t.Fatal("cancelled execution acknowledged")
	}
}

func TestCommandRejectsMissingAssembly(t *testing.T) {
	args := []string{"--job", id.New(sandbox.PrefixJob).String(), "--config", "/etc/cortex/worker.json"}
	if code := runCommand(t.Context(), args, "/run/credentials/worker", commandPorts{}); code == 0 {
		t.Fatal("missing assembly accepted")
	}
}
