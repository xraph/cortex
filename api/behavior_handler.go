package api

import (
	"fmt"
	"net/http"

	"github.com/xraph/forge"

	"github.com/xraph/cortex"
	"github.com/xraph/cortex/behavior"
	"github.com/xraph/cortex/id"
)

func (a *API) registerBehaviorRoutes(router forge.Router) error {
	g := router.Group("/cortex", forge.WithGroupTags("behaviors"))

	if err := g.POST("/behaviors", a.createBehavior,
		forge.WithSummary("Create behavior"),
		forge.WithDescription("Creates a new condition-triggered behavior pattern."),
		forge.WithOperationID("createBehavior"),
		forge.WithRequestSchema(CreateBehaviorRequest{}),
		forge.WithCreatedResponse(&behavior.Behavior{}),
		forge.WithErrorResponses(),
	); err != nil {
		return fmt.Errorf("register behavior routes: %w", err)
	}

	if err := g.GET("/behaviors", a.listBehaviors,
		forge.WithSummary("List behaviors"),
		forge.WithOperationID("listBehaviors"),
		forge.WithRequestSchema(ListBehaviorsRequest{}),
		forge.WithResponseSchema(http.StatusOK, "Behavior list", []*behavior.Behavior{}),
		forge.WithErrorResponses(),
	); err != nil {
		return fmt.Errorf("register behavior routes: %w", err)
	}

	if err := g.GET("/behaviors/:name", a.getBehavior,
		forge.WithSummary("Get behavior"),
		forge.WithOperationID("getBehavior"),
		forge.WithResponseSchema(http.StatusOK, "Behavior details", &behavior.Behavior{}),
		forge.WithErrorResponses(),
	); err != nil {
		return fmt.Errorf("register behavior routes: %w", err)
	}

	if err := g.PUT("/behaviors/:name", a.updateBehavior,
		forge.WithSummary("Update behavior"),
		forge.WithOperationID("updateBehavior"),
		forge.WithRequestSchema(UpdateBehaviorRequest{}),
		forge.WithResponseSchema(http.StatusOK, "Updated behavior", &behavior.Behavior{}),
		forge.WithErrorResponses(),
	); err != nil {
		return fmt.Errorf("register behavior routes: %w", err)
	}

	if err := g.DELETE("/behaviors/:name", a.deleteBehavior,
		forge.WithSummary("Delete behavior"),
		forge.WithOperationID("deleteBehavior"),
		forge.WithNoContentResponse(),
		forge.WithErrorResponses(),
	); err != nil {
		return fmt.Errorf("register behavior routes: %w", err)
	}

	return nil
}

func (a *API) createBehavior(ctx forge.Context, req *CreateBehaviorRequest) (*behavior.Behavior, error) {
	if req.Name == "" {
		return nil, forge.BadRequest("name is required")
	}

	triggers := make([]behavior.Trigger, len(req.Triggers))
	for i, t := range req.Triggers {
		triggers[i] = behavior.Trigger{
			Type:    behavior.TriggerType(t.Type),
			Pattern: t.Pattern,
		}
	}

	actions := make([]behavior.Action, len(req.Actions))
	for i, act := range req.Actions {
		actions[i] = behavior.Action{
			Type:   behavior.ActionType(act.Type),
			Target: act.Target,
			Value:  act.Value,
		}
	}

	b := &behavior.Behavior{
		Entity:        cortex.NewEntity(),
		ID:            id.NewBehaviorID(),
		Name:          req.Name,
		Description:   req.Description,
		AppID:         cortex.AppFromContext(ctx.Context()),
		Triggers:      triggers,
		Actions:       actions,
		Priority:      req.Priority,
		RequiresSkill: req.RequiresSkill,
		RequiresTrait: req.RequiresTrait,
		Metadata:      req.Metadata,
	}

	if err := a.eng.CreateBehavior(ctx.Context(), b); err != nil {
		return nil, fmt.Errorf("create behavior: %w", err)
	}

	return b, ctx.JSON(http.StatusCreated, b)
}

func (a *API) getBehavior(ctx forge.Context, _ *GetBehaviorRequest) (*behavior.Behavior, error) {
	b, err := a.eng.GetBehaviorByName(ctx.Context(), cortex.AppFromContext(ctx.Context()), ctx.Param("name"))
	if err != nil {
		return nil, mapStoreError(err)
	}
	return b, ctx.JSON(http.StatusOK, b)
}

func (a *API) listBehaviors(ctx forge.Context, req *ListBehaviorsRequest) ([]*behavior.Behavior, error) {
	behaviors, err := a.eng.ListBehaviors(ctx.Context(), &behavior.ListFilter{
		AppID:  cortex.AppFromContext(ctx.Context()),
		Limit:  defaultLimit(req.Limit),
		Offset: req.Offset,
	})
	if err != nil {
		return nil, fmt.Errorf("list behaviors: %w", err)
	}
	return behaviors, ctx.JSON(http.StatusOK, behaviors)
}

func (a *API) updateBehavior(ctx forge.Context, req *UpdateBehaviorRequest) (*behavior.Behavior, error) {
	b, err := a.eng.GetBehaviorByName(ctx.Context(), cortex.AppFromContext(ctx.Context()), req.Name)
	if err != nil {
		return nil, mapStoreError(err)
	}

	if req.Description != "" {
		b.Description = req.Description
	}
	if req.Priority > 0 {
		b.Priority = req.Priority
	}
	if req.RequiresSkill != "" {
		b.RequiresSkill = req.RequiresSkill
	}
	if req.RequiresTrait != "" {
		b.RequiresTrait = req.RequiresTrait
	}
	if req.Metadata != nil {
		b.Metadata = req.Metadata
	}
	if req.Triggers != nil {
		triggers := make([]behavior.Trigger, len(req.Triggers))
		for i, t := range req.Triggers {
			triggers[i] = behavior.Trigger{
				Type:    behavior.TriggerType(t.Type),
				Pattern: t.Pattern,
			}
		}
		b.Triggers = triggers
	}
	if req.Actions != nil {
		actions := make([]behavior.Action, len(req.Actions))
		for i, act := range req.Actions {
			actions[i] = behavior.Action{
				Type:   behavior.ActionType(act.Type),
				Target: act.Target,
				Value:  act.Value,
			}
		}
		b.Actions = actions
	}

	if err := a.eng.UpdateBehavior(ctx.Context(), b); err != nil {
		return nil, fmt.Errorf("update behavior: %w", err)
	}
	return b, ctx.JSON(http.StatusOK, b)
}

func (a *API) deleteBehavior(ctx forge.Context, _ *DeleteBehaviorRequest) (*struct{}, error) {
	b, err := a.eng.GetBehaviorByName(ctx.Context(), cortex.AppFromContext(ctx.Context()), ctx.Param("name"))
	if err != nil {
		return nil, mapStoreError(err)
	}
	if err := a.eng.DeleteBehavior(ctx.Context(), b.ID); err != nil {
		return nil, mapStoreError(err)
	}
	return nil, ctx.NoContent(http.StatusNoContent)
}
