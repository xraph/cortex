package agent

import (
	"encoding/json"
	"fmt"

	"github.com/xraph/cortex"
	"github.com/xraph/cortex/id"
)

// CloneConfig returns an independent deep copy of src as a new agent with the
// given ID and name and fresh timestamps. All other configuration (including
// AppID, Enabled, and PersonaRef) is preserved. Runtime history is not a field
// on Config and is therefore never copied. The deep copy is a JSON round-trip,
// so all nested slices and maps are independent of the source.
func CloneConfig(src *Config, newID id.AgentID, newName string) (*Config, error) {
	data, err := json.Marshal(src)
	if err != nil {
		return nil, fmt.Errorf("clone agent config: marshal: %w", err)
	}
	clone := new(Config)
	if err := json.Unmarshal(data, clone); err != nil {
		return nil, fmt.Errorf("clone agent config: unmarshal: %w", err)
	}
	clone.Entity = cortex.NewEntity()
	clone.ID = newID
	clone.Name = newName
	return clone, nil
}
