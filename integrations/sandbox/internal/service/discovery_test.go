package service

import (
	"context"
	"errors"
	"reflect"
	"testing"

	"github.com/godbus/dbus/v5"

	"github.com/xraph/cortex/id"
	"github.com/xraph/cortex/integrations/sandbox/internal/containment"
	"github.com/xraph/cortex/sandbox"
)

func discoveredUnit(key id.ID) []any {
	layout, _ := containment.ForJob(key)
	return []any{layout.Unit, "worker", "loaded", "active", "running", "", dbus.ObjectPath("/org/freedesktop/systemd1/unit/worker"), uint32(0), "", dbus.ObjectPath("/")}
}

func TestSystemdDiscoveryUsesReservedNamespaceAndAllStates(t *testing.T) {
	a, b := id.New(sandbox.PrefixJob), id.New(sandbox.PrefixJob)
	first, second := discoveredUnit(a), discoveredUnit(b)
	first[3], first[4], first[7], first[8], first[9] = "inactive", "dead", uint32(42), "start", dbus.ObjectPath("/org/freedesktop/systemd1/job/42")
	bus := &fakeBus{reply: func(busCall) ([]any, error) { return []any{[][]any{second, first}}, nil }}
	keys, err := (&systemManager{bus: bus}).Workers(t.Context(), 2)
	if err != nil || !reflect.DeepEqual(keys, []id.ID{a, b}) {
		t.Fatal("lost loaded or queued worker", keys, err)
	}
	want := []busCall{{managerPath, managerInterface + "ListUnitsByPatterns", []any{[]string{}, []string{containment.WorkerUnitPattern}}}}
	if !reflect.DeepEqual(bus.calls, want) {
		t.Fatal("discovery broadened namespace or excluded queued/inactive services", bus.calls)
	}
}

func TestSystemdDiscoveryRejectsAmbiguousSnapshots(t *testing.T) {
	for _, mode := range []string{"foreign", "invalid", "following", "duplicate", "oversized", "shape", "error", "cancel", "limit"} {
		t.Run(mode, func(t *testing.T) {
			key := id.New(sandbox.PrefixJob)
			row := discoveredUnit(key)
			rows := [][]any{row}
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			maximum := 1
			switch mode {
			case "foreign":
				row[0] = "ssh.service"
			case "invalid":
				row[0] = "cortex-sandbox-worker@../escape.service"
			case "following":
				row[5] = "another.service"
			case "duplicate":
				rows = append(rows, row)
				maximum = 2
			case "oversized":
				rows = append(rows, discoveredUnit(id.New(sandbox.PrefixJob)))
			case "shape":
				row[7] = "invalid job type"
			case "limit":
				maximum = 0
			}
			bus := &fakeBus{reply: func(busCall) ([]any, error) {
				if mode == "error" {
					return nil, errors.New("discovery unavailable")
				}
				if mode == "cancel" {
					cancel()
				}
				return []any{rows}, nil
			}}
			if keys, err := (&systemManager{bus: bus}).Workers(ctx, maximum); err == nil || keys != nil {
				t.Fatal("unsafe snapshot accepted", keys, err)
			}
		})
	}
}

func TestWorkerUnitIdentityRoundTripRejectsAliasesAndPaths(t *testing.T) {
	key := id.New(sandbox.PrefixJob)
	layout, err := containment.ForJob(key)
	if err != nil {
		t.Fatal(err)
	}
	if parsed, parseErr := containment.JobFromUnit(layout.Unit); parseErr != nil || parsed != key {
		t.Fatal(parsed, parseErr)
	}
	for _, unit := range []string{"", "ssh.service", "cortex-sandbox-worker@.service", layout.Unit + ".service", "/system.slice/" + layout.Unit, "cortex-sandbox-worker@" + id.New(id.PrefixAgent).String() + ".service"} {
		if _, parseErr := containment.JobFromUnit(unit); parseErr == nil {
			t.Fatal("noncanonical unit accepted", unit)
		}
	}
}
