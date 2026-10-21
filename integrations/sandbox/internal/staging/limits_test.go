package staging

import "testing"

func TestInputLimitsRequireFiniteBudgets(t *testing.T) {
	for _, limits := range []Limits{{PerInputBytes: 1 << 20, TotalBytes: 2 << 20}} {
		if err := limits.Validate(); err != nil {
			t.Fatal(err)
		}
	}
	for _, limits := range []Limits{{}, {PerInputBytes: 1, TotalBytes: 0}, {PerInputBytes: 1<<30 + 1, TotalBytes: 2 << 30}, {PerInputBytes: 1, TotalBytes: 4<<30 + 1}} {
		if err := limits.Validate(); err == nil {
			t.Fatal("unbounded input staging accepted")
		}
	}
}
