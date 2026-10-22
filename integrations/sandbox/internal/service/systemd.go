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

func (m *systemManager) Loaded(ctx context.Context, unit string) (map[string]any, error) {
	body, err := m.bus.Call(ctx, managerPath, managerInterface+"LoadUnit", unit)
	if err != nil {
		return nil, err
	}
	var object dbus.ObjectPath
	if decodeErr := dbus.Store(body, &object); decodeErr != nil {
		return nil, decodeErr
	}
	if !object.IsValid() || !strings.HasPrefix(string(object), "/org/freedesktop/systemd1/unit/") {
		return nil, fmt.Errorf("cortex: invalid loaded service object")
	}
	properties, err := m.properties(ctx, object, "")
	if err != nil {
		return nil, err
	}
	result := make(map[string]any, len(properties))
	for name, value := range properties {
		result[name] = value.Value()
	}
	return result, nil
}

func (m *systemManager) properties(ctx context.Context, object dbus.ObjectPath, iface string) (map[string]dbus.Variant, error) {
	body, err := m.bus.Call(ctx, object, "org.freedesktop.DBus.Properties.GetAll", iface)
	if err != nil {
		return nil, err
	}
	var properties map[string]dbus.Variant
	if decodeErr := dbus.Store(body, &properties); decodeErr != nil {
		return nil, decodeErr
	}
	return properties, nil
}

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
	return jobAcknowledgement(body)
}

func (m *systemManager) Start(ctx context.Context, unit string) error {
	body, err := m.bus.Call(ctx, managerPath, managerInterface+"StartUnit", unit, "fail")
	if err != nil {
		return err
	}
	return jobAcknowledgement(body)
}

func jobAcknowledgement(body []any) error {
	var job dbus.ObjectPath
	if operationErr := dbus.Store(body, &job); operationErr != nil {
		return operationErr
	}
	if !job.IsValid() || !strings.HasPrefix(string(job), "/org/freedesktop/systemd1/job/") {
		return fmt.Errorf("cortex: invalid service job acknowledgement")
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
	properties, err := m.properties(ctx, object, "org.freedesktop.systemd1.Unit")
	if err != nil {
		return false, err
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
