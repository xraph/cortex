package api

import (
	"fmt"
	"net/http"

	"github.com/xraph/forge"

	"github.com/xraph/cortex"
	"github.com/xraph/cortex/id"
	"github.com/xraph/cortex/trait"
)

func (a *API) registerTraitRoutes(router forge.Router) error {
	g := router.Group("/v1", forge.WithGroupTags("traits"))

	if err := g.POST("/traits", a.createTrait,
		forge.WithSummary("Create trait"),
		forge.WithDescription("Creates a new personality trait with dimensions and influences."),
		a.op("createTrait", true),
		forge.WithRequestSchema(CreateTraitRequest{}),
		forge.WithCreatedResponse(&trait.Trait{}),
		forge.WithErrorResponses(),
	); err != nil {
		return fmt.Errorf("register trait routes: %w", err)
	}

	if err := g.GET("/traits", a.listTraits,
		forge.WithSummary("List traits"),
		a.op("listTraits", false),
		forge.WithRequestSchema(ListTraitsRequest{}),
		forge.WithResponseSchema(http.StatusOK, "Trait list", []*trait.Trait{}),
		forge.WithErrorResponses(),
	); err != nil {
		return fmt.Errorf("register trait routes: %w", err)
	}

	if err := g.GET("/traits/:name", a.getTrait,
		forge.WithSummary("Get trait"),
		a.op("getTrait", false),
		forge.WithResponseSchema(http.StatusOK, "Trait details", &trait.Trait{}),
		forge.WithErrorResponses(),
	); err != nil {
		return fmt.Errorf("register trait routes: %w", err)
	}

	if err := g.PUT("/traits/:name", a.updateTrait,
		forge.WithSummary("Update trait"),
		a.op("updateTrait", true),
		forge.WithRequestSchema(UpdateTraitRequest{}),
		forge.WithResponseSchema(http.StatusOK, "Updated trait", &trait.Trait{}),
		forge.WithErrorResponses(),
	); err != nil {
		return fmt.Errorf("register trait routes: %w", err)
	}

	if err := g.DELETE("/traits/:name", a.deleteTrait,
		forge.WithSummary("Delete trait"),
		a.op("deleteTrait", true),
		forge.WithNoContentResponse(),
		forge.WithErrorResponses(),
	); err != nil {
		return fmt.Errorf("register trait routes: %w", err)
	}

	return nil
}

func (a *API) createTrait(ctx forge.Context, req *CreateTraitRequest) (*trait.Trait, error) {
	if req.Name == "" {
		return nil, forge.BadRequest("name is required")
	}

	dims := make([]trait.Dimension, len(req.Dimensions))
	for i, d := range req.Dimensions {
		dims[i] = trait.Dimension{
			Name:      d.Name,
			LowLabel:  d.LowLabel,
			HighLabel: d.HighLabel,
			Value:     d.Value,
		}
	}

	infls := make([]trait.Influence, len(req.Influences))
	for i, inf := range req.Influences {
		infls[i] = trait.Influence{
			Target:    trait.InfluenceTarget(inf.Target),
			Value:     inf.Value,
			Condition: inf.Condition,
			Weight:    inf.Weight,
		}
	}

	t := &trait.Trait{
		Entity:      cortex.NewEntity(),
		ID:          id.NewTraitID(),
		Name:        req.Name,
		Description: req.Description,
		Dimensions:  dims,
		Influences:  infls,
		Category:    trait.Category(req.Category),
		Metadata:    req.Metadata,
	}

	if err := a.eng.CreateTrait(ctx.Context(), t); err != nil {
		return nil, fmt.Errorf("create trait: %w", err)
	}

	return nil, ctx.JSON(http.StatusCreated, t)
}

func (a *API) getTrait(ctx forge.Context, _ *GetTraitRequest) (*trait.Trait, error) {
	t, err := a.eng.GetTraitByName(ctx.Context(), ctx.Param("name"))
	if err != nil {
		return nil, mapStoreError(err)
	}
	return t, nil
}

func (a *API) listTraits(ctx forge.Context, req *ListTraitsRequest) (*ListTraitsResponse, error) {
	traits, err := a.eng.ListTraits(ctx.Context(), &trait.ListFilter{
		Category: trait.Category(req.Category),
		Limit:    defaultLimit(req.Limit),
		Offset:   req.Offset,
	})
	if err != nil {
		return nil, fmt.Errorf("list traits: %w", err)
	}
	resp := &ListTraitsResponse{Items: traits}
	return resp, nil
}

func (a *API) updateTrait(ctx forge.Context, req *UpdateTraitRequest) (*trait.Trait, error) {
	t, err := a.eng.GetTraitByName(ctx.Context(), req.Name)
	if err != nil {
		return nil, mapStoreError(err)
	}

	if req.Description != "" {
		t.Description = req.Description
	}
	if req.Category != "" {
		t.Category = trait.Category(req.Category)
	}
	if req.Metadata != nil {
		t.Metadata = req.Metadata
	}
	if req.Dimensions != nil {
		dims := make([]trait.Dimension, len(req.Dimensions))
		for i, d := range req.Dimensions {
			dims[i] = trait.Dimension{
				Name:      d.Name,
				LowLabel:  d.LowLabel,
				HighLabel: d.HighLabel,
				Value:     d.Value,
			}
		}
		t.Dimensions = dims
	}
	if req.Influences != nil {
		infls := make([]trait.Influence, len(req.Influences))
		for i, inf := range req.Influences {
			infls[i] = trait.Influence{
				Target:    trait.InfluenceTarget(inf.Target),
				Value:     inf.Value,
				Condition: inf.Condition,
				Weight:    inf.Weight,
			}
		}
		t.Influences = infls
	}

	if err := a.eng.UpdateTrait(ctx.Context(), t); err != nil {
		return nil, fmt.Errorf("update trait: %w", err)
	}
	return t, nil
}

func (a *API) deleteTrait(ctx forge.Context, _ *DeleteTraitRequest) (*struct{}, error) {
	t, err := a.eng.GetTraitByName(ctx.Context(), ctx.Param("name"))
	if err != nil {
		return nil, mapStoreError(err)
	}
	if err := a.eng.DeleteTrait(ctx.Context(), t.ID); err != nil {
		return nil, mapStoreError(err)
	}
	return nil, ctx.NoContent(http.StatusNoContent)
}
