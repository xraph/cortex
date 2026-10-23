package planning

import (
	"github.com/xraph/cortex/execution"
	"github.com/xraph/cortex/id"
)

// Authority is a bounded capability ceiling, not a credential or a reservation.
// A host must resolve current permissions before trusting one from storage.
type Authority struct {
	Resources []ResourceGrant               `json:"resources,omitempty"`
	Effects   []execution.EffectRequirement `json:"effects,omitempty"`
	Budget    execution.Budget              `json:"budget"`
}

// Validate checks declarations without authenticating their issuer.
func (a Authority) Validate() error {
	if !boundedBudget(a.Budget) || validateResources(a.Resources) != nil {
		return ErrInvalid
	}
	if _, err := effectManifest(a.Effects); err != nil {
		return err
	}
	for _, effect := range a.Effects {
		if !effectResourcesGranted(effect, a.Resources) {
			return ErrInvalid
		}
	}

	return nil
}

// Within checks attenuation, preserving pinned resource and handler revisions.
func (a Authority) Within(parent Authority) bool {
	return a.Validate() == nil && parent.Validate() == nil && budgetWithin(a.Budget, parent.Budget) && resourcesWithin(a.Resources, parent.Resources) && effectsWithin(a.Effects, parent.Effects)
}

// TaskAuthority returns capabilities declared for one task in a sealed plan.
func TaskAuthority(p Plan, taskID id.ID) (Authority, error) {
	if Validate(p, Limits{}) != nil {
		return Authority{}, ErrInvalid
	}
	for _, task := range p.Tasks {
		if task.ID != taskID {
			continue
		}
		a := Authority{Resources: append([]ResourceGrant(nil), task.Resources...), Budget: task.Budget}
		for _, name := range task.Effects {
			for _, effect := range p.Effects {
				if effect.Tool == name {
					a.Effects = append(a.Effects, effect)
					break
				}
			}
		}
		return a, nil
	}
	return Authority{}, ErrInvalid
}
