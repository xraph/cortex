package optimization

import "testing"

func TestUsageDoesNotInventKnownZeroCost(t *testing.T) {
	for _, usage := range []Usage{{}, {MoneyQuality: UsageUnknown}, {MoneyQuality: UsageEstimated, USDMicros: 12, PricingRevision: "p1"}} {
		if _, known := usage.KnownCost(); known {
			t.Fatal("absent or estimated cost treated as known")
		}
	}
	free := Usage{TokenQuality: UsageKnown, MoneyQuality: UsageKnown, PricingRevision: "fixture-zero-price"}
	if cost, known := free.KnownCost(); !known || cost != 0 {
		t.Fatal("explicit zero-priced fixture lost")
	}
	for _, usage := range []Usage{
		{MoneyQuality: UsageKnown},
		{TokenQuality: "perfect"},
		{Tokens: -1},
		{MoneyQuality: UsageKnown, USDMicros: -1, PricingRevision: "p1"},
	} {
		if err := usage.Validate(); err == nil {
			t.Fatalf("invalid usage accepted: %+v", usage)
		}
	}
}
