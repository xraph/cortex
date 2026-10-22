package service

import (
	"context"
	"errors"
	"reflect"
	"testing"

	"github.com/godbus/dbus/v5"
)

type busCall struct {
	object dbus.ObjectPath
	method string
	args   []any
}
type fakeBus struct {
	calls []busCall
	reply func(busCall) ([]any, error)
}

func (b *fakeBus) Call(_ context.Context, object dbus.ObjectPath, method string, args ...any) ([]any, error) {
	c := busCall{object, method, args}
	b.calls = append(b.calls, c)
	return b.reply(c)
}
func (*fakeBus) Close() error { return nil }

func TestSystemdUsesPersistentNonReplacingMaskAndStopReplacement(t *testing.T) {
	b := &fakeBus{reply: func(c busCall) ([]any, error) {
		if c.method == managerInterface+"StopUnit" {
			return []any{dbus.ObjectPath("/org/freedesktop/systemd1/job/42")}, nil
		}
		return nil, nil
	}}
	m := &systemManager{bus: b}
	if err := m.Mask(t.Context(), "one.service"); err != nil {
		t.Fatal(err)
	}
	if err := m.Reload(t.Context()); err != nil {
		t.Fatal(err)
	}
	if err := m.Stop(t.Context(), "one.service"); err != nil {
		t.Fatal(err)
	}
	want := []busCall{{managerPath, managerInterface + "MaskUnitFiles", []any{[]string{"one.service"}, false, false}}, {managerPath, managerInterface + "Reload", nil}, {managerPath, managerInterface + "StopUnit", []any{"one.service", "replace"}}}
	if !reflect.DeepEqual(b.calls, want) {
		t.Fatalf("unsafe service operations: %#v", b.calls)
	}
}

func stateProperties() map[string]dbus.Variant {
	return map[string]dbus.Variant{"Id": dbus.MakeVariant("one.service"), "LoadState": dbus.MakeVariant("masked"), "Transient": dbus.MakeVariant(false), "Following": dbus.MakeVariant(""), "ActiveState": dbus.MakeVariant("inactive"), "Job": dbus.MakeVariant([]any{uint32(0), dbus.ObjectPath("/")})}
}

func TestSystemdRequiresExactQuiescentServiceState(t *testing.T) {
	for _, scenario := range []string{"inactive", "failed", "cached loaded", "transient", "running", "pending", "alias", "following", "missing", "bad job type", "bad job path", "bad object", "read denied"} {
		t.Run(scenario, func(t *testing.T) {
			properties := stateProperties()
			switch scenario {
			case "cached loaded":
				properties["LoadState"] = dbus.MakeVariant("loaded")
			case "transient":
				properties["Transient"] = dbus.MakeVariant(true)
			case "failed":
				properties["ActiveState"] = dbus.MakeVariant("failed")
			case "running":
				properties["ActiveState"] = dbus.MakeVariant("active")
			case "pending":
				properties["Job"] = dbus.MakeVariant([]any{uint32(8), dbus.ObjectPath("/org/freedesktop/systemd1/job/8")})
			case "alias":
				properties["Id"] = dbus.MakeVariant("another.service")
			case "following":
				properties["Following"] = dbus.MakeVariant("another.service")
			case "missing":
				delete(properties, "Job")
			case "bad job type":
				properties["Job"] = dbus.MakeVariant([]any{"0", dbus.ObjectPath("/")})
			case "bad job path":
				properties["Job"] = dbus.MakeVariant([]any{uint32(0), "/"})
			}
			b := &fakeBus{reply: func(c busCall) ([]any, error) {
				if c.method == managerInterface+"GetUnit" {
					if scenario == "bad object" {
						return []any{dbus.ObjectPath("/")}, nil
					}
					return []any{dbus.ObjectPath("/org/freedesktop/systemd1/unit/one_2eservice")}, nil
				}
				if scenario == "read denied" {
					return nil, errors.New("denied")
				}
				return []any{properties}, nil
			}}
			stopped, err := (&systemManager{bus: b}).Stopped(t.Context(), "one.service")
			want := scenario == "inactive" || scenario == "failed"
			if stopped != want || (want && err != nil) {
				t.Fatalf("unproved service state: %v %v", stopped, err)
			}
		})
	}
}

func TestSystemdOnlyAcceptsExplicitAbsenceAndPersistentMask(t *testing.T) {
	for _, failure := range []error{dbus.Error{Name: "org.freedesktop.systemd1.NoSuchUnit"}, &dbus.Error{Name: "org.freedesktop.systemd1.NoSuchUnit"}, dbus.Error{Name: "org.freedesktop.DBus.Error.AccessDenied"}, errors.New("not found")} {
		b := &fakeBus{reply: func(busCall) ([]any, error) { return nil, failure }}
		m := &systemManager{bus: b}
		stopped, err := m.Stopped(t.Context(), "one.service")
		if noSuchUnit(failure) {
			if !stopped || err != nil {
				t.Fatal("explicit absence rejected")
			}
		} else if stopped || err == nil {
			t.Fatal("error mistaken for absent service")
		}
	}
	for _, state := range []string{"masked", "masked-runtime", "disabled", "static", ""} {
		b := &fakeBus{reply: func(busCall) ([]any, error) { return []any{state}, nil }}
		masked, err := (&systemManager{bus: b}).Masked(t.Context(), "one.service")
		if err != nil || masked != (state == "masked") {
			t.Fatalf("wrong persistent mask state: %q %v %v", state, masked, err)
		}
	}
}

func TestStartNeverReplacesAQueuedStopOrRetriesAnAmbiguousReply(t *testing.T) {
	for _, body := range [][]any{{dbus.ObjectPath("/org/freedesktop/systemd1/job/9")}, {dbus.ObjectPath("/")}, nil} {
		b := &fakeBus{reply: func(c busCall) ([]any, error) {
			if c.method != managerInterface+"StartUnit" || !reflect.DeepEqual(c.args, []any{"one.service", "fail"}) {
				t.Fatal("unsafe start request")
			}
			return body, nil
		}}
		err := (&systemManager{bus: b}).Start(t.Context(), "one.service")
		good := len(body) == 1 && body[0] == dbus.ObjectPath("/org/freedesktop/systemd1/job/9")
		if good != (err == nil) || len(b.calls) != 1 {
			t.Fatal("incorrect start acknowledgement or retry")
		}
	}
}
