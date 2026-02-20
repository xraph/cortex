package api

import (
	"fmt"
	"net/http"

	"github.com/xraph/cortex"
	"github.com/xraph/cortex/id"
	"github.com/xraph/cortex/persona"
	"github.com/xraph/cortex/skill"
	"github.com/xraph/forge"
)

func (a *API) registerPersonaRoutes(router forge.Router) {
	g := router.Group("/cortex", forge.WithGroupTags("personas"))

	_ = g.POST("/personas", a.createPersona,
		forge.WithSummary("Create persona"),
		forge.WithDescription("Creates a new persona composing skills, traits, behaviors, and styles."),
		forge.WithOperationID("createPersona"),
		forge.WithRequestSchema(CreatePersonaRequest{}),
		forge.WithCreatedResponse(&persona.Persona{}),
		forge.WithErrorResponses(),
	)

	_ = g.GET("/personas", a.listPersonas,
		forge.WithSummary("List personas"),
		forge.WithOperationID("listPersonas"),
		forge.WithRequestSchema(ListPersonasRequest{}),
		forge.WithResponseSchema(http.StatusOK, "Persona list", []*persona.Persona{}),
		forge.WithErrorResponses(),
	)

	_ = g.GET("/personas/:name", a.getPersona,
		forge.WithSummary("Get persona"),
		forge.WithOperationID("getPersona"),
		forge.WithResponseSchema(http.StatusOK, "Persona details", &persona.Persona{}),
		forge.WithErrorResponses(),
	)

	_ = g.PUT("/personas/:name", a.updatePersona,
		forge.WithSummary("Update persona"),
		forge.WithOperationID("updatePersona"),
		forge.WithRequestSchema(UpdatePersonaRequest{}),
		forge.WithResponseSchema(http.StatusOK, "Updated persona", &persona.Persona{}),
		forge.WithErrorResponses(),
	)

	_ = g.DELETE("/personas/:name", a.deletePersona,
		forge.WithSummary("Delete persona"),
		forge.WithOperationID("deletePersona"),
		forge.WithNoContentResponse(),
		forge.WithErrorResponses(),
	)
}

func (a *API) createPersona(ctx forge.Context, req *CreatePersonaRequest) (*persona.Persona, error) {
	if req.Name == "" {
		return nil, forge.BadRequest("name is required")
	}

	skills := make([]persona.SkillAssignment, len(req.Skills))
	for i, s := range req.Skills {
		skills[i] = persona.SkillAssignment{
			SkillName:   s.SkillName,
			Proficiency: skill.Proficiency(s.Proficiency),
		}
	}

	traits := make([]persona.TraitAssignment, len(req.Traits))
	for i, t := range req.Traits {
		traits[i] = persona.TraitAssignment{
			TraitName:       t.TraitName,
			DimensionValues: t.DimensionValues,
		}
	}

	p := &persona.Persona{
		Entity:      cortex.NewEntity(),
		ID:          id.NewPersonaID(),
		Name:        req.Name,
		Description: req.Description,
		AppID:       cortex.AppFromContext(ctx.Context()),
		Identity:    req.Identity,
		Skills:      skills,
		Traits:      traits,
		Behaviors:   req.Behaviors,
		Metadata:    req.Metadata,
	}

	if err := a.eng.CreatePersona(ctx.Context(), p); err != nil {
		return nil, fmt.Errorf("create persona: %w", err)
	}

	return p, ctx.JSON(http.StatusCreated, p)
}

func (a *API) getPersona(ctx forge.Context, _ *GetPersonaRequest) (*persona.Persona, error) {
	p, err := a.eng.GetPersonaByName(ctx.Context(), cortex.AppFromContext(ctx.Context()), ctx.Param("name"))
	if err != nil {
		return nil, mapStoreError(err)
	}
	return p, ctx.JSON(http.StatusOK, p)
}

func (a *API) listPersonas(ctx forge.Context, req *ListPersonasRequest) ([]*persona.Persona, error) {
	personas, err := a.eng.ListPersonas(ctx.Context(), &persona.ListFilter{
		AppID:  cortex.AppFromContext(ctx.Context()),
		Limit:  defaultLimit(req.Limit),
		Offset: req.Offset,
	})
	if err != nil {
		return nil, fmt.Errorf("list personas: %w", err)
	}
	return personas, ctx.JSON(http.StatusOK, personas)
}

func (a *API) updatePersona(ctx forge.Context, req *UpdatePersonaRequest) (*persona.Persona, error) {
	p, err := a.eng.GetPersonaByName(ctx.Context(), cortex.AppFromContext(ctx.Context()), req.Name)
	if err != nil {
		return nil, mapStoreError(err)
	}

	if req.Description != "" {
		p.Description = req.Description
	}
	if req.Identity != "" {
		p.Identity = req.Identity
	}
	if req.Behaviors != nil {
		p.Behaviors = req.Behaviors
	}
	if req.Metadata != nil {
		p.Metadata = req.Metadata
	}
	if req.Skills != nil {
		skills := make([]persona.SkillAssignment, len(req.Skills))
		for i, s := range req.Skills {
			skills[i] = persona.SkillAssignment{
				SkillName:   s.SkillName,
				Proficiency: skill.Proficiency(s.Proficiency),
			}
		}
		p.Skills = skills
	}
	if req.Traits != nil {
		traits := make([]persona.TraitAssignment, len(req.Traits))
		for i, t := range req.Traits {
			traits[i] = persona.TraitAssignment{
				TraitName:       t.TraitName,
				DimensionValues: t.DimensionValues,
			}
		}
		p.Traits = traits
	}

	if err := a.eng.UpdatePersona(ctx.Context(), p); err != nil {
		return nil, fmt.Errorf("update persona: %w", err)
	}
	return p, ctx.JSON(http.StatusOK, p)
}

func (a *API) deletePersona(ctx forge.Context, _ *DeletePersonaRequest) (*struct{}, error) {
	p, err := a.eng.GetPersonaByName(ctx.Context(), cortex.AppFromContext(ctx.Context()), ctx.Param("name"))
	if err != nil {
		return nil, mapStoreError(err)
	}
	if err := a.eng.DeletePersona(ctx.Context(), p.ID); err != nil {
		return nil, mapStoreError(err)
	}
	return nil, ctx.NoContent(http.StatusNoContent)
}
