package optimization

import (
	"fmt"
	"strings"
)

// UsageQuality describes accounting provenance, independently of result quality.
type UsageQuality string

const (
	UsageKnown     UsageQuality = "known"
	UsageEstimated UsageQuality = "estimated"
	UsageUnknown   UsageQuality = "unknown"
)

// EvidenceClass describes which claims a comparison can support.
type EvidenceClass string

const (
	EvidenceValidated   EvidenceClass = "validated"
	EvidenceContrastive EvidenceClass = "contrastive"
	EvidenceExploratory EvidenceClass = "exploratory"
)

// Usage keeps token and money provenance separate. Missing quality is unknown.
type Usage struct {
	Tokens          int64        `json:"tokens"`
	TokenQuality    UsageQuality `json:"token_quality"`
	USDMicros       int64        `json:"usd_micros"`
	MoneyQuality    UsageQuality `json:"money_quality"`
	PricingRevision string       `json:"pricing_revision,omitempty"`
	Receipt         string       `json:"receipt,omitempty"`
}

// Validate refuses negative quantities and unqualified priced receipts.
func (usage Usage) Validate() error {
	if usage.Tokens < 0 || usage.USDMicros < 0 {
		return fmt.Errorf("%w: negative usage", ErrInvalidSpec)
	}
	for _, quality := range []UsageQuality{usage.TokenQuality, usage.MoneyQuality} {
		if quality != "" && quality != UsageUnknown && quality != UsageKnown && quality != UsageEstimated {
			return fmt.Errorf("%w: invalid usage quality", ErrInvalidSpec)
		}
	}
	if (usage.MoneyQuality == UsageKnown || usage.MoneyQuality == UsageEstimated) && strings.TrimSpace(usage.PricingRevision) == "" {
		return fmt.Errorf("%w: priced usage lacks revision", ErrInvalidSpec)
	}
	return nil
}

// KnownCost exposes a comparable price only when its provenance is explicit.
func (usage Usage) KnownCost() (int64, bool) {
	return usage.USDMicros, usage.MoneyQuality == UsageKnown && usage.Validate() == nil
}
