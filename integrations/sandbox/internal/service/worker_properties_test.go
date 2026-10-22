package service

import (
	"context"
	"errors"
	"os"
	"testing"

	"github.com/godbus/dbus/v5"

	"github.com/xraph/cortex/id"
	"github.com/xraph/cortex/sandbox"
)

func TestWorkerActivationIsPendingOnlyAfterProfileAndIdentityChecks(t *testing.T) {
	p := profileFixture(t)
	key := id.New(sandbox.PrefixJob)
	for _, activating := range []bool{true, false} {
		properties := runningProperties(p, key)
		properties["Job"] = []any{uint32(42), dbus.ObjectPath("/org/freedesktop/systemd1/job/42")}
		if activating {
			properties["ActiveState"], properties["SubState"] = "activating", "start"
		}
		if err := checkWorkerProperties(p, key, properties, 123); !errors.Is(err, errWorkerActivating) {
			t.Fatalf("worker activation did not remain pending: %v", err)
		}
		for name, value := range map[string]any{"MainPID": uint32(124), "NRestarts": uint32(1), "MemoryMax": uint64(1), "Job": []any{uint32(42), dbus.ObjectPath("/org/freedesktop/systemd1/job/43")}} {
			original := properties[name]
			properties[name] = value
			if err := checkWorkerProperties(p, key, properties, 123); err == nil || errors.Is(err, errWorkerActivating) {
				t.Fatalf("changed %s treated as safe activation: %v", name, err)
			}
			properties[name] = original
		}
	}
}

func TestWorkerActivationSignalSurvivesDeploymentVerification(t *testing.T) {
	p := profileFixture(t)
	key := id.New(sandbox.PrefixJob)
	for _, cancelled := range []bool{false, true} {
		ctx, cancel := context.WithCancel(t.Context())
		load := func(context.Context, string) (map[string]any, error) {
			properties := runningProperties(p, key)
			properties["ActiveState"], properties["SubState"] = "activating", "start"
			properties["Job"] = []any{uint32(42), dbus.ObjectPath("/org/freedesktop/systemd1/job/42")}
			if cancelled {
				cancel()
			}
			return properties, nil
		}
		err := checkDeployment(ctx, p, key, uint32(os.Geteuid()), 123, load)
		cancel()
		if cancelled {
			if !errors.Is(err, context.Canceled) {
				t.Fatal("cancelled deployment check lost cancellation", err)
			}
		} else if err != errWorkerActivating { //nolint:errorlint // The wire permits only this sole signal, not a joined failure.
			t.Fatalf("deployment verification wrapped the sole activation signal: %v", err)
		}
	}
}

func TestRunningWorkerAdmissionRechecksPinnedFiles(t *testing.T) {
	p := profileFixture(t)
	key := id.New(sandbox.PrefixJob)
	load := func(_ context.Context, unit string) (map[string]any, error) {
		if unit != "cortex-sandbox-worker@"+key.String()+".service" {
			t.Fatal("foreign service loaded")
		}
		return runningProperties(p, key), nil
	}
	if err := checkDeployment(t.Context(), p, key, uint32(os.Geteuid()), 123, load); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p.Config.Path, []byte("changed"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := checkDeployment(t.Context(), p, key, uint32(os.Geteuid()), 123, load); err == nil {
		t.Fatal("changed running-worker configuration authenticated")
	}
}

func runningProperties(p LaunchProfile, key id.ID) map[string]any {
	properties := launchProperties(p, key)
	properties["ActiveState"] = "active"
	properties["SubState"] = "running"
	properties["ControlGroup"] = "/system.slice/cortex-sandbox-worker@" + key.String() + ".service"
	properties["MainPID"] = uint32(123)
	properties["ExecMainPID"] = uint32(123)
	properties["ControlPID"] = uint32(0)
	properties["NRestarts"] = uint32(0)
	properties["ExecMainStartTimestamp"] = uint64(123456)
	properties["ExecMainStartTimestampMonotonic"] = uint64(1234)
	properties["ExecMainExitTimestamp"] = uint64(0)
	properties["ExecMainExitTimestampMonotonic"] = uint64(0)
	properties["ExecMainCode"] = int32(0)
	properties["ExecMainStatus"] = int32(0)
	properties["ExecStart"] = [][]any{{p.Worker.Path, []string{p.Worker.Path, "--job", key.String(), "--config", p.Config.Path}, false, uint64(123456), uint64(1234), uint64(0), uint64(0), uint32(123), int32(0), int32(0)}}
	return properties
}

func TestRunningWorkerRequiresCurrentMainProcessAndApprovedProfile(t *testing.T) {
	p := profileFixture(t)
	key := id.New(sandbox.PrefixJob)
	if err := checkWorkerProperties(p, key, runningProperties(p, key), 123); err != nil {
		t.Fatalf("approved running worker refused: %v", err)
	}
	for name, value := range map[string]any{
		"MainPID": uint32(124), "ExecMainPID": uint32(124), "ControlPID": uint32(124), "NRestarts": uint32(1),
		"ActiveState": "activating", "SubState": "start", "ControlGroup": "/system.slice/foreign.service",
		"ExecMainExitTimestamp": uint64(1), "ExecMainStartTimestamp": uint64(0), "ExecMainCode": int32(1),
		"MemoryMax": uint64(1), "DelegateSubgroup": "payload", "Environment": []string{"EVIL=yes"},
	} {
		t.Run(name, func(t *testing.T) {
			properties := runningProperties(p, key)
			properties[name] = value
			if err := checkWorkerProperties(p, key, properties, 123); err == nil {
				t.Fatal("changed running worker authenticated")
			}
		})
	}
	if err := checkWorkerProperties(p, key, runningProperties(p, key), 124); err == nil {
		t.Fatal("helper process authenticated as main worker")
	}
}

func TestRunningWorkerRejectsStaleOrMalformedExecutionRecord(t *testing.T) {
	p := profileFixture(t)
	key := id.New(sandbox.PrefixJob)
	for index, value := range map[int]any{0: "/evil", 1: []string{"/evil"}, 2: true, 3: uint64(0), 4: uint64(0), 5: uint64(1), 6: uint64(1), 7: uint32(124), 8: int32(1), 9: int32(1)} {
		properties := runningProperties(p, key)
		properties["ExecStart"].([][]any)[0][index] = value
		if err := checkWorkerProperties(p, key, properties, 123); err == nil {
			t.Fatalf("execution field %d bypassed binding", index)
		}
	}
	for _, pid := range []uint32{0, 1} {
		if err := checkWorkerProperties(p, key, runningProperties(p, key), pid); err == nil {
			t.Fatal("invalid main process accepted")
		}
	}
}
