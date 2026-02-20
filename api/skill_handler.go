package api

import (
	"fmt"
	"net/http"

	"github.com/xraph/cortex"
	"github.com/xraph/cortex/id"
	"github.com/xraph/cortex/skill"
	"github.com/xraph/forge"
)

func (a *API) registerSkillRoutes(router forge.Router) {
	g := router.Group("/cortex", forge.WithGroupTags("skills"))

	_ = g.POST("/skills", a.createSkill,
		forge.WithSummary("Create skill"),
		forge.WithDescription("Creates a new skill with tool bindings, knowledge refs, and proficiency config."),
		forge.WithOperationID("createSkill"),
		forge.WithRequestSchema(CreateSkillRequest{}),
		forge.WithCreatedResponse(&skill.Skill{}),
		forge.WithErrorResponses(),
	)

	_ = g.GET("/skills", a.listSkills,
		forge.WithSummary("List skills"),
		forge.WithOperationID("listSkills"),
		forge.WithRequestSchema(ListSkillsRequest{}),
		forge.WithResponseSchema(http.StatusOK, "Skill list", []*skill.Skill{}),
		forge.WithErrorResponses(),
	)

	_ = g.GET("/skills/:name", a.getSkill,
		forge.WithSummary("Get skill"),
		forge.WithOperationID("getSkill"),
		forge.WithResponseSchema(http.StatusOK, "Skill details", &skill.Skill{}),
		forge.WithErrorResponses(),
	)

	_ = g.PUT("/skills/:name", a.updateSkill,
		forge.WithSummary("Update skill"),
		forge.WithOperationID("updateSkill"),
		forge.WithRequestSchema(UpdateSkillRequest{}),
		forge.WithResponseSchema(http.StatusOK, "Updated skill", &skill.Skill{}),
		forge.WithErrorResponses(),
	)

	_ = g.DELETE("/skills/:name", a.deleteSkill,
		forge.WithSummary("Delete skill"),
		forge.WithOperationID("deleteSkill"),
		forge.WithNoContentResponse(),
		forge.WithErrorResponses(),
	)
}

func (a *API) createSkill(ctx forge.Context, req *CreateSkillRequest) (*skill.Skill, error) {
	if req.Name == "" {
		return nil, forge.BadRequest("name is required")
	}

	tools := make([]skill.ToolBinding, len(req.Tools))
	for i, t := range req.Tools {
		tools[i] = skill.ToolBinding{
			ToolName:   t.ToolName,
			Mastery:    skill.Proficiency(t.Mastery),
			Guidance:   t.Guidance,
			PreferWhen: t.PreferWhen,
		}
	}

	knowledge := make([]skill.KnowledgeRef, len(req.Knowledge))
	for i, k := range req.Knowledge {
		knowledge[i] = skill.KnowledgeRef{
			Source:     k.Source,
			InjectMode: k.InjectMode,
			Priority:   k.Priority,
		}
	}

	s := &skill.Skill{
		Entity:               cortex.NewEntity(),
		ID:                   id.NewSkillID(),
		Name:                 req.Name,
		Description:          req.Description,
		AppID:                cortex.AppFromContext(ctx.Context()),
		Tools:                tools,
		Knowledge:            knowledge,
		SystemPromptFragment: req.SystemPromptFragment,
		Dependencies:         req.Dependencies,
		DefaultProficiency:   skill.Proficiency(req.DefaultProficiency),
		Metadata:             req.Metadata,
	}

	if err := a.eng.CreateSkill(ctx.Context(), s); err != nil {
		return nil, fmt.Errorf("create skill: %w", err)
	}

	return s, ctx.JSON(http.StatusCreated, s)
}

func (a *API) getSkill(ctx forge.Context, _ *GetSkillRequest) (*skill.Skill, error) {
	s, err := a.eng.GetSkillByName(ctx.Context(), cortex.AppFromContext(ctx.Context()), ctx.Param("name"))
	if err != nil {
		return nil, mapStoreError(err)
	}
	return s, ctx.JSON(http.StatusOK, s)
}

func (a *API) listSkills(ctx forge.Context, req *ListSkillsRequest) ([]*skill.Skill, error) {
	skills, err := a.eng.ListSkills(ctx.Context(), &skill.ListFilter{
		AppID:  cortex.AppFromContext(ctx.Context()),
		Limit:  defaultLimit(req.Limit),
		Offset: req.Offset,
	})
	if err != nil {
		return nil, fmt.Errorf("list skills: %w", err)
	}
	return skills, ctx.JSON(http.StatusOK, skills)
}

func (a *API) updateSkill(ctx forge.Context, req *UpdateSkillRequest) (*skill.Skill, error) {
	s, err := a.eng.GetSkillByName(ctx.Context(), cortex.AppFromContext(ctx.Context()), req.Name)
	if err != nil {
		return nil, mapStoreError(err)
	}

	if req.Description != "" {
		s.Description = req.Description
	}
	if req.SystemPromptFragment != "" {
		s.SystemPromptFragment = req.SystemPromptFragment
	}
	if req.Dependencies != nil {
		s.Dependencies = req.Dependencies
	}
	if req.DefaultProficiency != "" {
		s.DefaultProficiency = skill.Proficiency(req.DefaultProficiency)
	}
	if req.Metadata != nil {
		s.Metadata = req.Metadata
	}
	if req.Tools != nil {
		tools := make([]skill.ToolBinding, len(req.Tools))
		for i, t := range req.Tools {
			tools[i] = skill.ToolBinding{
				ToolName:   t.ToolName,
				Mastery:    skill.Proficiency(t.Mastery),
				Guidance:   t.Guidance,
				PreferWhen: t.PreferWhen,
			}
		}
		s.Tools = tools
	}
	if req.Knowledge != nil {
		knowledge := make([]skill.KnowledgeRef, len(req.Knowledge))
		for i, k := range req.Knowledge {
			knowledge[i] = skill.KnowledgeRef{
				Source:     k.Source,
				InjectMode: k.InjectMode,
				Priority:   k.Priority,
			}
		}
		s.Knowledge = knowledge
	}

	if err := a.eng.UpdateSkill(ctx.Context(), s); err != nil {
		return nil, fmt.Errorf("update skill: %w", err)
	}
	return s, ctx.JSON(http.StatusOK, s)
}

func (a *API) deleteSkill(ctx forge.Context, _ *DeleteSkillRequest) (*struct{}, error) {
	s, err := a.eng.GetSkillByName(ctx.Context(), cortex.AppFromContext(ctx.Context()), ctx.Param("name"))
	if err != nil {
		return nil, mapStoreError(err)
	}
	if err := a.eng.DeleteSkill(ctx.Context(), s.ID); err != nil {
		return nil, mapStoreError(err)
	}
	return nil, ctx.NoContent(http.StatusNoContent)
}
