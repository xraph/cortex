package image

import (
	"testing"
	"time"
)

func TestImageLimitsRequireFiniteBudgets(t *testing.T) {
	valid := Limits{CompressedBytes: 1 << 20, ExpandedBytes: 2 << 20, Entries: 100, Timeout: time.Minute}
	if err := valid.Validate(); err != nil {
		t.Fatal(err)
	}
	for _, change := range []func(*Limits){func(l *Limits) { l.CompressedBytes = 0 }, func(l *Limits) { l.CompressedBytes = 4<<30 + 1 }, func(l *Limits) { l.ExpandedBytes = 0 }, func(l *Limits) { l.ExpandedBytes = 8<<30 + 1 }, func(l *Limits) { l.Entries = 0 }, func(l *Limits) { l.Entries = 100001 }, func(l *Limits) { l.Timeout = 0 }, func(l *Limits) { l.Timeout = 11 * time.Minute }} {
		limits := valid
		change(&limits)
		if err := limits.Validate(); err == nil {
			t.Fatal("unbounded image staging accepted")
		}
	}
}
