//go:build linux || darwin

package main

import (
	"context"
	"encoding/json"
	"io"

	"github.com/xraph/cortex/evidence"
	"github.com/xraph/cortex/integrations/sandbox/internal/service"
	"github.com/xraph/cortex/sandbox"
)

// runCommand emits only a verified installation receipt. Diagnostic errors can
// contain source material and never reach stdout or stderr.
func runCommand(ctx context.Context, args []string, install func(context.Context, service.InputProvisionRequest) (service.InputInstallation, error), output io.Writer) int {
	if ctx.Err() != nil || install == nil || output == nil || len(args) != 8 || args[0] != "--config" || args[2] != "--job" || args[4] != "--input" || args[6] != "--file" {
		return 1
	}
	for _, value := range []string{args[1], args[3], args[5], args[7]} {
		if value == "" {
			return 1
		}
	}
	receipt, err := install(ctx, service.InputProvisionRequest{Config: args[1], Job: args[3], Input: args[5], File: args[7]})
	if err != nil || ctx.Err() != nil || receipt.SchemaVersion != 1 || receipt.JobID.IsNil() || receipt.JobID.Prefix() != sandbox.PrefixJob || !evidence.ValidDigest(receipt.JobDigest) || !evidence.ValidDigest(receipt.BindingDigest) || !evidence.ValidDigest(receipt.SHA256) || receipt.Bytes < 0 || receipt.Bytes > 1<<20 {
		return 1
	}
	if err = json.NewEncoder(output).Encode(receipt); err != nil {
		return 1
	}
	return 0
}
