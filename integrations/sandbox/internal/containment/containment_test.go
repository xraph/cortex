package containment

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/xraph/cortex/id"
	"github.com/xraph/cortex/sandbox"
)

func TestLayoutAndMembershipBindTheWholeJobService(t *testing.T) {
	key := id.New(sandbox.PrefixJob)
	layout, err := ForJob(key)
	if err != nil {
		t.Fatal(err)
	}
	if layout.Unit != "cortex-sandbox-worker@"+key.String()+".service" || layout.Parent != "/system.slice/"+layout.Unit || layout.Worker != layout.Parent+"/supervisor" || layout.Payload != layout.Parent+"/payload" {
		t.Fatalf("unbound containment: %+v", layout)
	}
	if err = CheckMembership(key, []byte("0::"+layout.Worker+"\n")); err != nil {
		t.Fatal(err)
	}
	for _, body := range []string{"", "0::/\n", "0::" + layout.Parent + "\n", "0::" + layout.Payload + "\n", "0::" + layout.Worker + "/child\n", "0::" + layout.Worker + "\n1:cpu:/legacy\n", "0::" + layout.Worker + "/../supervisor\n", strings.Repeat("x", 8193)} {
		if err = CheckMembership(key, []byte(body)); err == nil {
			t.Fatalf("uncontained membership accepted: %q", body)
		}
	}
	if _, err = ForJob(id.New("agt")); err == nil {
		t.Fatal("non-job target accepted")
	}
}

type fixtureGroup struct {
	reads                int
	killed               bool
	killErr, errorOnRead error
	events               [][]byte
	afterRead            func()
}

func (f *fixtureGroup) Kill(context.Context, id.ID) error { f.killed = true; return f.killErr }
func (f *fixtureGroup) Events(context.Context, id.ID) ([]byte, error) {
	f.reads++
	if f.afterRead != nil {
		f.afterRead()
	}
	if f.errorOnRead != nil {
		return nil, f.errorOnRead
	}
	i := min(f.reads-1, len(f.events)-1)
	return f.events[i], nil
}

func TestCancellationCannotPublishALateCleanupObservation(t *testing.T) {
	for _, absent := range []bool{false, true} {
		ctx, cancel := context.WithCancel(t.Context())
		f := &fixtureGroup{events: [][]byte{[]byte("populated 0\n")}, afterRead: cancel}
		if absent {
			f.errorOnRead = ErrAbsent
		}
		observation, err := killAndWait(ctx, id.New(sandbox.PrefixJob), f)
		cancel()
		if !errors.Is(err, context.Canceled) || observation != (Observation{}) {
			t.Fatalf("late cleanup observation: %+v %v", observation, err)
		}
	}
}

func TestInvalidOrCancelledKillNeverTouchesBackend(t *testing.T) {
	f := &fixtureGroup{}
	if _, err := killAndWait(t.Context(), id.New("agt"), f); err == nil || f.killed {
		t.Fatal("invalid kill target reached backend")
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if _, err := killAndWait(ctx, id.New(sandbox.PrefixJob), f); !errors.Is(err, context.Canceled) || f.killed {
		t.Fatal("cancelled kill reached backend")
	}
}

func TestKillWaitsForEmptyDescendantTree(t *testing.T) {
	key := id.New(sandbox.PrefixJob)
	f := &fixtureGroup{events: [][]byte{[]byte("populated 1\nfrozen 0\n"), []byte("populated 0\nfrozen 0\n")}}
	observation, err := killAndWait(t.Context(), key, f)
	if err != nil || !f.killed || f.reads != 2 || !observation.Empty || observation.Absent {
		t.Fatalf("unconfirmed cleanup: %+v %+v %v", f, observation, err)
	}
}

func TestAbsentGroupIsAnObservationNotAStartFence(t *testing.T) {
	key := id.New(sandbox.PrefixJob)
	f := &fixtureGroup{killErr: ErrAbsent, errorOnRead: ErrAbsent}
	observation, err := killAndWait(t.Context(), key, f)
	if err != nil || !observation.Absent || observation.Empty {
		t.Fatalf("absence conflated with fenced cleanup: %+v %v", observation, err)
	}
	f = &fixtureGroup{killErr: ErrAbsent, events: [][]byte{[]byte("populated 0\n")}}
	if _, err = killAndWait(t.Context(), key, f); !errors.Is(err, ErrChanged) {
		t.Fatalf("new group appeared after absent kill: %v", err)
	}
}

func TestUnconfirmedKillNeverReportsEmpty(t *testing.T) {
	key := id.New(sandbox.PrefixJob)
	for _, test := range []string{"kill error", "read error", "malformed", "populated timeout"} {
		t.Run(test, func(t *testing.T) {
			f := &fixtureGroup{events: [][]byte{[]byte("populated 1\n")}}
			switch test {
			case "kill error":
				f.killErr = errors.New("kill denied")
			case "read error":
				f.errorOnRead = errors.New("read denied")
			case "malformed":
				f.events = [][]byte{[]byte("populated 2\n")}
			}
			ctx, cancel := context.WithTimeout(t.Context(), 20*time.Millisecond)
			defer cancel()
			observation, err := killAndWait(ctx, key, f)
			if err == nil || observation.Empty || observation.Absent {
				t.Fatalf("unproved cleanup: %+v %v", observation, err)
			}
		})
	}
}

func TestEventsRejectAmbiguousOrUnboundedInput(t *testing.T) {
	for _, body := range []string{"", "frozen 0\n", "populated -1\n", "populated 0\npopulated 1\n", "populated 0 extra\n", "populated zero\n", strings.Repeat("x", 4097)} {
		if _, err := parseEvents([]byte(body)); err == nil {
			t.Fatalf("invalid cgroup events accepted: %q", body)
		}
	}
}
