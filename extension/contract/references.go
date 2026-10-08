package contract

import (
	"context"

	"github.com/xraph/forge/extensions/dashboard/contract/dispatcher"

	"github.com/xraph/cortex"
	"github.com/xraph/cortex/agent"
	"github.com/xraph/cortex/behavior"
	"github.com/xraph/cortex/id"
	"github.com/xraph/cortex/persona"
	"github.com/xraph/cortex/skill"
	"github.com/xraph/cortex/trait"
)

type ReferenceInput struct {
	ListInput
	Kind      string `json:"kind"`
	OwnerKind string `json:"owner_kind"`
	OwnerID   string `json:"owner_id"`
}
type Reference struct {
	ID    string       `json:"id"`
	Name  string       `json:"name"`
	Scope cortex.Scope `json:"scope"`
}

func (s *service) bindReferences(d *dispatcher.Dispatcher) error {
	return query(d, s, "references.list", func(ctx context.Context, in ReferenceInput) (ListOutput[Reference], error) {
		out := ListOutput[Reference]{Items: []*Reference{}}
		if err := in.normalize(); err != nil {
			return out, err
		}
		if in.OwnerID != "" {
			v, err := id.Parse(in.OwnerID)
			if err != nil {
				return out, bad("Invalid owner ID")
			}
			var scope cortex.Scope
			switch in.OwnerKind {
			case "agents":
				row, e := s.deps.Engine.GetAgent(ctx, v)
				err = e
				if row != nil {
					scope = row.Scope
				}
			case "personas":
				row, e := s.deps.Engine.GetPersona(ctx, v)
				err = e
				if row != nil {
					scope = row.Scope
				}
			case "skills":
				row, e := s.deps.Engine.GetSkill(ctx, v)
				err = e
				if row != nil {
					scope = row.Scope
				}
			case "traits":
				row, e := s.deps.Engine.GetTrait(ctx, v)
				err = e
				if row != nil {
					scope = row.Scope
				}
			case "behaviors":
				row, e := s.deps.Engine.GetBehavior(ctx, v)
				err = e
				if row != nil {
					scope = row.Scope
				}
			case "orchestrations":
				row, e := s.deps.Engine.GetOrchestration(ctx, v)
				err = e
				if row != nil {
					scope = row.Scope
				}
			default:
				return out, bad("Invalid owner kind")
			}
			if err != nil {
				return out, err
			}
			ctx = cortex.WithScope(ctx, scope)
		}
		add := func(rawID, name string, scope cortex.Scope) {
			out.Items = append(out.Items, &Reference{rawID, name, scope})
		}
		var err error
		switch in.Kind {
		case "agents":
			f := &agent.ListFilter{Search: in.Search, Limit: in.Limit, Offset: in.Offset, Exact: true}
			rows, e := s.deps.Engine.ListAgents(ctx, f)
			err = e
			if err == nil {
				out.Total, err = s.deps.Engine.CountAgents(ctx, f)
			}
			for _, r := range rows {
				add(r.ID.String(), r.Name, r.Scope)
			}
		case "personas":
			f := &persona.ListFilter{Search: in.Search, Limit: in.Limit, Offset: in.Offset, Exact: true}
			rows, e := s.deps.Engine.ListPersonas(ctx, f)
			err = e
			if err == nil {
				out.Total, err = s.deps.Engine.CountPersonas(ctx, f)
			}
			for _, r := range rows {
				add(r.ID.String(), r.Name, r.Scope)
			}
		case "skills":
			f := &skill.ListFilter{Search: in.Search, Limit: in.Limit, Offset: in.Offset, Exact: true}
			rows, e := s.deps.Engine.ListSkills(ctx, f)
			err = e
			if err == nil {
				out.Total, err = s.deps.Engine.CountSkills(ctx, f)
			}
			for _, r := range rows {
				add(r.ID.String(), r.Name, r.Scope)
			}
		case "traits":
			f := &trait.ListFilter{Search: in.Search, Limit: in.Limit, Offset: in.Offset, Exact: true}
			rows, e := s.deps.Engine.ListTraits(ctx, f)
			err = e
			if err == nil {
				out.Total, err = s.deps.Engine.CountTraits(ctx, f)
			}
			for _, r := range rows {
				add(r.ID.String(), r.Name, r.Scope)
			}
		case "behaviors":
			f := &behavior.ListFilter{Search: in.Search, Limit: in.Limit, Offset: in.Offset, Exact: true}
			rows, e := s.deps.Engine.ListBehaviors(ctx, f)
			err = e
			if err == nil {
				out.Total, err = s.deps.Engine.CountBehaviors(ctx, f)
			}
			for _, r := range rows {
				add(r.ID.String(), r.Name, r.Scope)
			}
		default:
			return out, bad("Invalid reference kind")
		}
		out.Limit = in.Limit
		out.Offset = in.Offset
		return out, err
	})
}
