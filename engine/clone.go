package engine

import (
	"context"
	"errors"
	"fmt"

	"github.com/xraph/cortex"
	"github.com/xraph/cortex/agent"
	"github.com/xraph/cortex/id"
	"github.com/xraph/cortex/persona"
)

// maxCloneNameAttempts caps auto-generated name probing to avoid an unbounded loop.
const maxCloneNameAttempts = 1000

// resolveCloneName returns a free name for a clone. If desired is non-empty it is
// used when free and rejected with ErrAlreadyExists when taken. If desired is
// empty, "<source>-copy", "<source>-copy-2", … is tried until a free name is found.
// exists reports whether a name is already in use (true), free (false), or errors.
func resolveCloneName(ctx context.Context, desired, source string, exists func(context.Context, string) (bool, error)) (string, error) {
	if desired != "" {
		taken, err := exists(ctx, desired)
		if err != nil {
			return "", err
		}
		if taken {
			return "", cortex.ErrAlreadyExists
		}
		return desired, nil
	}

	base := source + "-copy"
	candidate := base
	for i := 1; i <= maxCloneNameAttempts; i++ {
		taken, err := exists(ctx, candidate)
		if err != nil {
			return "", err
		}
		if !taken {
			return candidate, nil
		}
		candidate = fmt.Sprintf("%s-%d", base, i+1)
	}
	return "", fmt.Errorf("clone: no free name for %q after %d attempts", source, maxCloneNameAttempts)
}

// CloneAgent creates an independent copy of an existing agent in the same app.
func (e *Engine) CloneAgent(ctx context.Context, appID, sourceName, newName string) (*agent.Config, error) {
	if e.store == nil {
		return nil, cortex.ErrNoStore
	}
	src, err := e.store.GetByName(ctx, appID, sourceName)
	if err != nil {
		return nil, err
	}
	name, err := resolveCloneName(ctx, newName, sourceName, func(c context.Context, n string) (bool, error) {
		_, gerr := e.store.GetByName(c, appID, n)
		if gerr == nil {
			return true, nil
		}
		if errors.Is(gerr, cortex.ErrAgentNotFound) {
			return false, nil
		}
		return false, gerr
	})
	if err != nil {
		return nil, err
	}
	clone, err := agent.CloneConfig(src, id.NewAgentID(), name)
	if err != nil {
		return nil, err
	}
	if err := e.store.Create(ctx, clone); err != nil {
		return nil, err
	}
	return clone, nil
}

// ClonePersona creates an independent copy of an existing persona in the same app.
func (e *Engine) ClonePersona(ctx context.Context, appID, sourceName, newName string) (*persona.Persona, error) {
	if e.store == nil {
		return nil, cortex.ErrNoStore
	}
	src, err := e.store.GetPersonaByName(ctx, appID, sourceName)
	if err != nil {
		return nil, err
	}
	name, err := resolveCloneName(ctx, newName, sourceName, func(c context.Context, n string) (bool, error) {
		_, gerr := e.store.GetPersonaByName(c, appID, n)
		if gerr == nil {
			return true, nil
		}
		if errors.Is(gerr, cortex.ErrPersonaNotFound) {
			return false, nil
		}
		return false, gerr
	})
	if err != nil {
		return nil, err
	}
	clone, err := persona.ClonePersona(src, id.NewPersonaID(), name)
	if err != nil {
		return nil, err
	}
	if err := e.store.CreatePersona(ctx, clone); err != nil {
		return nil, err
	}
	return clone, nil
}
