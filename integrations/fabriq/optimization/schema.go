package optimization

import (
	"github.com/xraph/fabriq/core/agent"
	"github.com/xraph/fabriq/core/registry"

	"github.com/xraph/cortex/integrations/fabriq/internal/lessontransport"
)

func validEntity(value string) bool { return lessontransport.ValidEntity(value) }

// LessonSpec shares the physical lesson schema across governed producers.
func LessonSpec(name string) registry.EntitySpec { return lessontransport.Spec(name) }

// LessonWritePolicy permits guarded creation and revision of this entity.
func LessonWritePolicy(name string) agent.WritePolicy { return lessontransport.WritePolicy(name) }
