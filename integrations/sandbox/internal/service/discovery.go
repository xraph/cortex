package service

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/godbus/dbus/v5"

	"github.com/xraph/cortex/id"
	"github.com/xraph/cortex/integrations/sandbox/internal/containment"
	"github.com/xraph/cortex/sandbox"
)

type workerDiscovery interface {
	Workers(context.Context, int) ([]id.ID, error)
}

// listedUnit matches systemd's ListUnitsByPatterns a(ssssssouso) reply.
type listedUnit struct {
	Name, Description, LoadState, ActiveState, SubState, Following string
	Path                                                           dbus.ObjectPath
	JobID                                                          uint32
	JobType                                                        string
	JobPath                                                        dbus.ObjectPath
}

func (m *systemManager) Workers(ctx context.Context, maximum int) ([]id.ID, error) {
	if maximum < 1 || maximum > 10000 {
		return nil, fmt.Errorf("cortex: invalid worker discovery limit")
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	body, err := m.bus.Call(ctx, managerPath, managerInterface+"ListUnitsByPatterns", []string{}, []string{containment.WorkerUnitPattern})
	if err != nil {
		return nil, err
	}
	var units []listedUnit
	if decodeErr := dbus.Store(body, &units); decodeErr != nil {
		return nil, decodeErr
	}
	if len(units) > maximum {
		return nil, fmt.Errorf("cortex: worker discovery limit exceeded")
	}
	keys := make([]id.ID, 0, len(units))
	for _, unit := range units {
		key, parseErr := containment.JobFromUnit(unit.Name)
		if parseErr != nil || unit.Following != "" {
			return nil, fmt.Errorf("cortex: ambiguous discovered worker identity")
		}
		keys = append(keys, key)
	}
	return validateDiscoveredWorkers(ctx, keys, maximum)
}

// Workers snapshots loaded worker services, including inactive units and units
// with queued jobs. The trusted manager reply must fit maximum (1..10000); an
// oversized response fails instead of silently hiding later workers. systemd's
// method is not paginated, so this bounds accepted rows after D-Bus decoding.
func (r *Reaper) Workers(ctx context.Context, maximum int) ([]id.ID, error) {
	source, ok := r.manager.(workerDiscovery)
	if !ok || maximum < 1 || maximum > 10000 {
		return nil, fmt.Errorf("cortex: invalid worker discovery service")
	}
	operation, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	if err := operation.Err(); err != nil {
		return nil, err
	}
	keys, err := source.Workers(operation, maximum)
	if err != nil {
		return nil, err
	}
	return validateDiscoveredWorkers(operation, keys, maximum)
}

func validateDiscoveredWorkers(ctx context.Context, keys []id.ID, maximum int) ([]id.ID, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if len(keys) > maximum {
		return nil, fmt.Errorf("cortex: worker discovery limit exceeded")
	}
	keys = slices.Clone(keys)
	slices.SortFunc(keys, func(a, b id.ID) int { return strings.Compare(a.String(), b.String()) })
	for i, key := range keys {
		if key.IsNil() || key.Prefix() != sandbox.PrefixJob || (i > 0 && keys[i-1] == key) {
			return nil, fmt.Errorf("cortex: invalid or duplicate discovered worker")
		}
	}
	return keys, nil
}
