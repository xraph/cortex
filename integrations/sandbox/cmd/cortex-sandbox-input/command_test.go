//go:build linux || darwin

package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"strings"
	"testing"

	"github.com/xraph/cortex/evidence"
	"github.com/xraph/cortex/id"
	"github.com/xraph/cortex/integrations/sandbox/internal/service"
	"github.com/xraph/cortex/sandbox"
)

func TestInputCommandPrintsOnlyInstallationReceipt(t *testing.T) {
	args := []string{"--config", "/etc/cortex/broker.json", "--job", "/etc/cortex/job.json", "--input", "/inputs/data", "--file", "/etc/cortex/data.bin"}
	expected := service.InputInstallation{SchemaVersion: 1, JobID: id.New(sandbox.PrefixJob), JobDigest: evidence.Digest([]byte("job")), BindingDigest: evidence.Digest([]byte("ref")), SHA256: evidence.Digest([]byte("data")), Bytes: 4}
	calls := 0
	install := func(ctx context.Context, request service.InputProvisionRequest) (service.InputInstallation, error) {
		calls++
		if request.Config != args[1] || request.Job != args[3] || request.Input != args[5] || request.File != args[7] {
			t.Fatal("changed invocation", request)
		}
		return expected, ctx.Err()
	}
	var output bytes.Buffer
	if code := runCommand(t.Context(), args, install, &output); code != 0 || calls != 1 {
		t.Fatal("command failed", code, calls)
	}
	var receipt service.InputInstallation
	if err := json.Unmarshal(output.Bytes(), &receipt); err != nil || receipt != expected {
		t.Fatal("invalid receipt output", err)
	}
	for _, mode := range []string{"arguments", "installer", "cancellation", "writer"} {
		t.Run(mode, func(t *testing.T) {
			invocation := append([]string(nil), args...)
			ctx := t.Context()
			execute := install
			var result bytes.Buffer
			var writer io.Writer = &result
			switch mode {
			case "arguments":
				invocation = append(invocation, "--override")
			case "installer":
				execute = func(context.Context, service.InputProvisionRequest) (service.InputInstallation, error) {
					return service.InputInstallation{}, errors.New("private source secret")
				}
			case "cancellation":
				var cancel context.CancelFunc
				ctx, cancel = context.WithCancel(ctx)
				cancel()
			case "writer":
				writer = inputFailedWriter{}
			}
			if code := runCommand(ctx, invocation, execute, writer); code == 0 {
				t.Fatal("failure succeeded")
			}
			if strings.Contains(result.String(), "secret") || result.Len() != 0 {
				t.Fatal("failure disclosed output")
			}
		})
	}
}

type inputFailedWriter struct{}

func (inputFailedWriter) Write([]byte) (int, error) { return 0, io.ErrClosedPipe }
