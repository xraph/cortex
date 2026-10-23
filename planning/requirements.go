package planning

import (
	"github.com/xraph/cortex/execution"
	"github.com/xraph/cortex/verification"
)

func validateRequirements(p Plan) error {
	if len(p.Effects) > 256 || verification.ValidateSources(p.Sources) != nil {
		return invalid("effect or source manifest")
	}
	effects, err := effectManifest(p.Effects)
	if err != nil {
		return err
	}
	sources := make(map[string]verification.Source, len(p.Sources))
	resources := resourceMap(p.Resources)
	for _, source := range p.Sources {
		grant, ok := resources[source.Resource]
		if !ok || grant.Revision != source.Revision {
			return invalid("source exceeds resource grant")
		}
		sources[source.Resource] = source
	}
	covered := make(map[string]bool, len(sources))
	for _, task := range p.Tasks {
		if len(task.Effects) > len(effects) || len(task.Sources) > len(sources) {
			return invalid("task manifest size")
		}
		grants := resourceMap(task.Resources)
		seen := make(map[string]bool, len(task.Effects))
		for _, name := range task.Effects {
			effect, ok := effects[name]
			if !ok || seen[name] {
				return invalid("unknown or repeated task effect")
			}
			seen[name] = true
			if !effectResourcesGranted(effect, task.Resources) {
				return invalid("mutation requires a writable resource")
			}
		}
		seen = make(map[string]bool, len(task.Sources))
		for _, name := range task.Sources {
			source, ok := sources[name]
			grant, granted := grants[name]
			if !ok || !granted || grant.Revision != source.Revision || seen[name] {
				return invalid("task source binding")
			}
			seen[name] = true
			covered[name] = true
		}
	}
	for name := range sources {
		if !covered[name] {
			return invalid("captured source has no consuming task")
		}
	}
	return nil
}

func resourceMap(resources []ResourceGrant) map[string]ResourceGrant {
	out := make(map[string]ResourceGrant, len(resources))
	for _, grant := range resources {
		out[grant.Resource] = grant
	}
	return out
}

func effectsWithin(child, parent []execution.EffectRequirement) bool {
	allowed := make(map[string]execution.EffectRequirement, len(parent))
	for _, effect := range parent {
		allowed[effect.Tool] = effect
	}
	for _, effect := range child {
		ceiling, ok := allowed[effect.Tool]
		if !ok || effect != ceiling {
			return false
		}
	}
	return true
}

func sourcesEqual(a, b []verification.Source) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if !a[i].Equal(b[i]) {
			return false
		}
	}
	return true
}

func taskContextMatches(p Plan, task Task, current verification.ContextSnapshot) bool {
	if len(current.Tools) != len(task.Effects) || len(current.Sources) != len(task.Sources) {
		return false
	}
	tools := make(map[string]string, len(current.Tools))
	for _, tool := range current.Tools {
		tools[tool.Name] = tool.Revision
	}
	effects := make(map[string]string, len(p.Effects))
	for _, effect := range p.Effects {
		effects[effect.Tool] = effect.Revision
	}
	for _, name := range task.Effects {
		if tools[name] != effects[name] {
			return false
		}
	}
	sources := make(map[string]verification.Source, len(current.Sources))
	for _, source := range current.Sources {
		sources[source.Resource] = source
	}
	pinned := make(map[string]verification.Source, len(p.Sources))
	for _, source := range p.Sources {
		pinned[source.Resource] = source
	}
	for _, name := range task.Sources {
		if !sources[name].Equal(pinned[name]) {
			return false
		}
	}
	return true
}

func effectManifest(declarations []execution.EffectRequirement) (map[string]execution.EffectRequirement, error) {
	if len(declarations) > 256 {
		return nil, invalid("effect manifest size")
	}
	effects := make(map[string]execution.EffectRequirement, len(declarations))
	for _, effect := range declarations {
		if effect.Validate() != nil {
			return nil, invalid("effect declaration")
		}
		if _, exists := effects[effect.Tool]; exists {
			return nil, invalid("duplicate effect")
		}
		effects[effect.Tool] = effect
	}
	return effects, nil
}
func effectResourcesGranted(effect execution.EffectRequirement, resources []ResourceGrant) bool {
	if effect.Class == execution.EffectReadOnly {
		return true
	}
	for _, grant := range resources {
		if grant.Access == AccessWrite {
			return true
		}
	}
	return false
}
