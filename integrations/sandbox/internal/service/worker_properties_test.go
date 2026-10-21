package service

import (
	"context"
	"os"
	"testing"

	"github.com/xraph/cortex/id"
	"github.com/xraph/cortex/sandbox"
)

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
