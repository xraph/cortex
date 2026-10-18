package optimization

import (
	"fmt"

	"github.com/xraph/cortex/id"
)

// Entity prefixes belong to this module and do not extend the root registry.
const (
	PrefixHarness    id.Prefix = "harn"
	PrefixVersion    id.Prefix = "hrev"
	PrefixExperiment id.Prefix = "oexp"
	PrefixCandidate  id.Prefix = "ocan"
	PrefixTrial      id.Prefix = "otrl"
	PrefixDecision   id.Prefix = "odec"
	PrefixLesson     id.Prefix = "oles"
	PrefixEvent      id.Prefix = "oevt"
	PrefixActivation id.Prefix = "oact"
	PrefixAttempt    id.Prefix = "oatt"
)

// ParseID validates both TypeID syntax and the expected entity kind.
func ParseID(value string, prefix id.Prefix) (id.ID, error) {
	parsed, err := id.ParseWithPrefix(value, prefix)
	if err != nil {
		return id.Nil, fmt.Errorf("%w: %w", ErrInvalidSpec, err)
	}
	return parsed, nil
}
