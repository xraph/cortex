package service

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/godbus/dbus/v5"
)

const managerPath dbus.ObjectPath = "/org/freedesktop/systemd1"
const managerInterface = "org.freedesktop.systemd1.Manager."

type bus interface {
	Call(context.Context, dbus.ObjectPath, string, ...any) ([]any, error)
	Close() error
}
type systemManager struct{ bus bus }

func (m *systemManager) Mask(ctx context.Context, unit string) error {
	_, err := m.bus.Call(ctx, managerPath, managerInterface+"MaskUnitFiles", []string{unit}, false, false)
	return err
}
func (m *systemManager) Reload(ctx context.Context) error {
	_, err := m.bus.Call(ctx, managerPath, managerInterface+"Reload")
	return err
}
func (m *systemManager) Masked(ctx context.Context, unit string) (bool, error) {
	body, err := m.bus.Call(ctx, managerPath, managerInterface+"GetUnitFileState", unit)
	if err != nil {
		return false, err
	}
	var state string
	if operationErr := dbus.Store(body, &state); operationErr != nil {
		return false, operationErr
	}
	return state == "masked", nil
}
func (m *systemManager) Stop(ctx context.Context, unit string) error {
	body, err := m.bus.Call(ctx, managerPath, managerInterface+"StopUnit", unit, "replace")
	if noSuchUnit(err) {
		return nil
	}
	if err != nil {
		return err
	}
	var job dbus.ObjectPath
	if operationErr := dbus.Store(body, &job); operationErr != nil {
		return operationErr
	}
	if !job.IsValid() || !strings.HasPrefix(string(job), "/org/freedesktop/systemd1/job/") {
		return fmt.Errorf("cortex: invalid stop job acknowledgement")
	}
	return nil
}
func (m *systemManager) Stopped(ctx context.Context, unit string) (bool, error) {
	body, err := m.bus.Call(ctx, managerPath, managerInterface+"GetUnit", unit)
	if noSuchUnit(err) {
		return true, nil
	}
	if err != nil {
		return false, err
	}
	var object dbus.ObjectPath
	if operationErr := dbus.Store(body, &object); operationErr != nil {
		return false, operationErr
	}
	if !object.IsValid() || !strings.HasPrefix(string(object), "/org/freedesktop/systemd1/unit/") {
		return false, fmt.Errorf("cortex: invalid service object")
	}
	body, err = m.bus.Call(ctx, object, "org.freedesktop.DBus.Properties.GetAll", "org.freedesktop.systemd1.Unit")
	if err != nil {
		return false, err
	}
	var properties map[string]dbus.Variant
	if operationErr := dbus.Store(body, &properties); operationErr != nil {
		return false, operationErr
	}
	name, nameOK := properties["Id"].Value().(string)
	following, followingOK := properties["Following"].Value().(string)
	active, activeOK := properties["ActiveState"].Value().(string)
	loaded, loadedOK := properties["LoadState"].Value().(string)
	transient, transientOK := properties["Transient"].Value().(bool)
	job, jobOK := properties["Job"].Value().([]any)
	if !nameOK || name != unit || !followingOK || following != "" || !activeOK || !jobOK || len(job) != 2 {
		return false, fmt.Errorf("cortex: ambiguous service state")
	}
	if !loadedOK || loaded != "masked" || !transientOK || transient {
		return false, fmt.Errorf("cortex: loaded service is not persistently fenced")
	}
	jobID, idOK := job[0].(uint32)
	jobPath, pathOK := job[1].(dbus.ObjectPath)
	if !idOK || !pathOK || !jobPath.IsValid() {
		return false, fmt.Errorf("cortex: invalid service job state")
	}
	return (active == "inactive" || active == "failed") && jobID == 0 && jobPath == "/", nil
}
func noSuchUnit(err error) bool {
	var value dbus.Error
	if errors.As(err, &value) {
		return value.Name == "org.freedesktop.systemd1.NoSuchUnit"
	}
	var pointer *dbus.Error
	return errors.As(err, &pointer) && pointer.Name == "org.freedesktop.systemd1.NoSuchUnit"
}
