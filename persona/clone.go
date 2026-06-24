package persona

import (
	"encoding/json"
	"fmt"

	"github.com/xraph/cortex"
	"github.com/xraph/cortex/id"
)

// ClonePersona returns an independent deep copy of src as a new persona with the
// given ID and name and fresh timestamps. All other fields (including AppID and
// the full skill/trait/behavior/style composition) are preserved. The deep copy
// is a JSON round-trip, so all nested slices and maps are independent of src.
func ClonePersona(src *Persona, newID id.PersonaID, newName string) (*Persona, error) {
	data, err := json.Marshal(src)
	if err != nil {
		return nil, fmt.Errorf("clone persona: marshal: %w", err)
	}
	clone := new(Persona)
	if err := json.Unmarshal(data, clone); err != nil {
		return nil, fmt.Errorf("clone persona: unmarshal: %w", err)
	}
	clone.Entity = cortex.NewEntity()
	clone.ID = newID
	clone.Name = newName
	return clone, nil
}
