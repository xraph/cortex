package service

import (
	"context"
	"errors"
	"reflect"
	"testing"
	"time"

	"github.com/xraph/cortex/id"
	"github.com/xraph/cortex/integrations/sandbox/internal/containment"
	"github.com/xraph/cortex/sandbox"
)

type fakeManager struct {
	calls           []string
	fail            string
	masked, stopped bool
	afterStop       func()
	afterCheck      func()
	checks          int
}

func (m *fakeManager) step(name string) error {
	m.calls = append(m.calls, name)
	if m.fail == name {
		return errors.New(name)
	}
	return nil
}
func (m *fakeManager) Mask(context.Context, string) error {
	if err := m.step("mask"); err != nil {
		return err
	}
	m.masked = true
	return nil
}
func (m *fakeManager) Reload(context.Context) error { return m.step("reload") }
func (m *fakeManager) Stop(context.Context, string) error {
	if err := m.step("stop"); err != nil {
		return err
	}
	m.stopped = true
	if m.afterStop != nil {
		m.afterStop()
	}
	return nil
}
func (m *fakeManager) Masked(context.Context, string) (bool, error) {
	return m.masked, m.step("masked")
}
func (m *fakeManager) Stopped(context.Context, string) (bool, error) {
	m.checks++
	if m.afterCheck != nil {
		m.afterCheck()
	}
	return m.stopped, m.step("stopped")
}

func TestReaperFencesBeforeStoppingAndRechecksAfterCleanup(t *testing.T) {
	m := &fakeManager{}
	r := &Reaper{manager: m, check: func(context.Context, id.ID) error { return nil }, kill: func(context.Context, id.ID) (containment.Observation, error) {
		m.calls = append(m.calls, "kill")
		return containment.Observation{Empty: true}, nil
	}}
	if err := r.FenceAndStop(t.Context(), id.New(sandbox.PrefixJob)); err != nil {
		t.Fatal(err)
	}
	want := []string{"mask", "reload", "masked", "stop", "kill", "stopped", "kill", "masked", "stopped"}
	if !reflect.DeepEqual(m.calls, want) {
		t.Fatalf("unsafe cleanup ordering: %v", m.calls)
	}
}

func TestReaperDoesNotTreatErrorsOrLostFenceAsProof(t *testing.T) {
	for _, failure := range []string{"mask", "reload", "masked", "stop", "kill", "stopped", "lost mask", "empty observation", "cancel"} {
		t.Run(failure, func(t *testing.T) {
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			m := &fakeManager{fail: failure}
			r := &Reaper{manager: m, check: func(context.Context, id.ID) error { return nil }, kill: func(context.Context, id.ID) (containment.Observation, error) {
				switch failure {
				case "kill":
					return containment.Observation{}, errors.New("kill failed")
				case "lost mask":
					m.masked = false
				case "empty observation":
					return containment.Observation{}, nil
				case "cancel":
					cancel()
				}
				return containment.Observation{Absent: true}, nil
			}}
			if err := r.FenceAndStop(ctx, id.New(sandbox.PrefixJob)); err == nil {
				t.Fatal("unconfirmed cleanup accepted")
			}
		})
	}
}

func TestReaperRefusesInvalidTargetsAndRemainsBounded(t *testing.T) {
	m := &fakeManager{}
	r := &Reaper{manager: m, check: func(context.Context, id.ID) error { return nil }, kill: func(context.Context, id.ID) (containment.Observation, error) {
		return containment.Observation{Empty: true}, nil
	}}
	if err := r.FenceAndStop(t.Context(), id.New("agt")); err == nil || len(m.calls) != 0 {
		t.Fatal("foreign target reached manager")
	}
	m.afterStop = func() { m.stopped = false }
	ctx, cancel := context.WithTimeout(t.Context(), 20*time.Millisecond)
	defer cancel()
	if err := r.FenceAndStop(ctx, id.New(sandbox.PrefixJob)); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("stuck service: %v", err)
	}
}

func TestReaperRetryNeverUnmasksIdentity(t *testing.T) {
	m := &fakeManager{fail: "stop"}
	r := &Reaper{manager: m, check: func(context.Context, id.ID) error { return nil }, kill: func(context.Context, id.ID) (containment.Observation, error) {
		return containment.Observation{Absent: true}, nil
	}}
	key := id.New(sandbox.PrefixJob)
	if err := r.FenceAndStop(t.Context(), key); err == nil || !m.masked {
		t.Fatal("lost durable mask")
	}
	m.fail = ""
	if err := r.FenceAndStop(t.Context(), key); err != nil {
		t.Fatal(err)
	}
}

func TestReaperDrainsDescendantsCreatedDuringStop(t *testing.T) {
	m := &fakeManager{}
	populated := true
	m.afterCheck = func() {
		if m.checks == 1 {
			populated = true
		}
	}
	r := &Reaper{manager: m, check: func(context.Context, id.ID) error { return nil }, kill: func(context.Context, id.ID) (containment.Observation, error) {
		populated = false
		return containment.Observation{Empty: true}, nil
	}}
	if err := r.FenceAndStop(t.Context(), id.New(sandbox.PrefixJob)); err != nil {
		t.Fatal(err)
	}
	if populated {
		t.Fatal("stop helper descendants remained after cleanup proof")
	}
}

func TestReaperRefusesItsOwnServiceBeforeMasking(t *testing.T) {
	m := &fakeManager{}
	denied := errors.New("inside target service")
	r := &Reaper{manager: m, check: func(context.Context, id.ID) error { return denied }, kill: func(context.Context, id.ID) (containment.Observation, error) {
		t.Fatal("self kill attempted")
		return containment.Observation{}, nil
	}}
	if err := r.FenceAndStop(t.Context(), id.New(sandbox.PrefixJob)); !errors.Is(err, denied) || len(m.calls) != 0 {
		t.Fatalf("self target reached manager: %v %v", m.calls, err)
	}
}

func TestReaperRejectsServiceChangeAfterLastDrain(t *testing.T) {
	m := &fakeManager{}
	m.afterCheck = func() {
		if m.checks == 2 {
			m.stopped = false
		}
	}
	r := &Reaper{manager: m, check: func(context.Context, id.ID) error { return nil }, kill: func(context.Context, id.ID) (containment.Observation, error) {
		return containment.Observation{Empty: true}, nil
	}}
	if err := r.FenceAndStop(t.Context(), id.New(sandbox.PrefixJob)); err == nil {
		t.Fatal("changed service accepted as reaped")
	}
}
