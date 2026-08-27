package a2a

import (
	"testing"
	"time"
)

func TestOptionsDefaults(t *testing.T) {
	got := Options{}.withDefaults()
	if got.HopCeiling != DefaultHopCeiling {
		t.Errorf("HopCeiling = %d, want %d", got.HopCeiling, DefaultHopCeiling)
	}
	if got.Workers != DefaultWorkers {
		t.Errorf("Workers = %d, want %d", got.Workers, DefaultWorkers)
	}
	if got.DefaultReplyBy != DefaultReplyBy {
		t.Errorf("DefaultReplyBy = %s, want %s", got.DefaultReplyBy, DefaultReplyBy)
	}
	if got.SweepInterval != DefaultSweepInterval {
		t.Errorf("SweepInterval = %s, want %s", got.SweepInterval, DefaultSweepInterval)
	}
	if got.DeliveryClaimTTL != DefaultDeliveryClaimTTL {
		t.Errorf("DeliveryClaimTTL = %s, want %s", got.DeliveryClaimTTL, DefaultDeliveryClaimTTL)
	}
}

func TestOptionsKeepExplicitValues(t *testing.T) {
	in := Options{HopCeiling: 2, Workers: 1, DefaultReplyBy: time.Second, SweepInterval: time.Minute, DeliveryClaimTTL: time.Hour}
	if got := in.withDefaults(); got != in {
		t.Fatalf("withDefaults changed explicit values: %+v", got)
	}
}

func TestOptionsRejectNegatives(t *testing.T) {
	got := Options{HopCeiling: -3, Workers: -1}.withDefaults()
	if got.HopCeiling != DefaultHopCeiling || got.Workers != DefaultWorkers {
		t.Fatalf("negatives must fall back to defaults, got %+v", got)
	}
}

func TestFakeClockDoesNotMoveOnItsOwn(t *testing.T) {
	c := &fakeClock{now: time.Unix(1000, 0).UTC()}
	first := c.Now()
	if !c.Now().Equal(first) {
		t.Fatal("the fake clock must not advance on its own")
	}
	c.advance(time.Minute)
	if !c.Now().Equal(first.Add(time.Minute)) {
		t.Fatal("advance must move the fake clock by exactly the delta")
	}
}

func TestSystemClockIsUTC(t *testing.T) {
	if got := (systemClock{}).Now(); got.Location() != time.UTC {
		t.Fatalf("Now() location = %s, want UTC", got.Location())
	}
}
