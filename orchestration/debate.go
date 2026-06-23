package orchestration

import (
	"context"
	"strconv"
	"strings"
)

type debate struct {
	runner   AgentRunner
	appID    string
	parts    []Participant
	settings Settings
}

func newDebate(runner AgentRunner, appID string, parts []Participant, settings Settings) *debate {
	return &debate{runner: runner, appID: appID, parts: parts, settings: settings}
}

func (o *debate) Strategy() string { return StrategyDebate }

func (o *debate) Run(ctx context.Context, input string, bb *Blackboard) (*Result, error) {
	res := &Result{OrchestrationID: bb.OrchestrationID(), Strategy: StrategyDebate}
	opts := runOptsFromSettings(o.settings)

	judge := o.resolveJudge()
	debaters := o.debaters(judge)
	rounds := o.settings.Rounds
	if rounds <= 0 {
		rounds = 1
	}

	last := ""
	for r := 0; r < rounds; r++ {
		for _, d := range debaters {
			prompt := buildDebatePrompt(input, bb.Snapshot(), r+1)
			ar, err := o.runner.RunAgent(ctx, o.appID, d.AgentName, prompt, opts)
			if err != nil {
				res.Err = err
				return res, err
			}
			bb.Append(d.AgentName, ar.Output)
			res.AgentResults = append(res.AgentResults, *ar)
			last = ar.Output
		}
	}

	if judge != "" {
		ar, err := o.runner.RunAgent(ctx, o.appID, judge, buildJudgePrompt(input, bb.Snapshot()), opts)
		if err != nil {
			res.Err = err
			res.Handoffs = bb.Handoffs()
			return res, err
		}
		res.AgentResults = append(res.AgentResults, *ar)
		res.Output = ar.Output
		res.Handoffs = bb.Handoffs()
		return res, nil
	}

	res.Output = last
	res.Handoffs = bb.Handoffs()
	return res, nil
}

func (o *debate) resolveJudge() string {
	if o.settings.Judge != "" {
		return o.settings.Judge
	}
	for _, p := range o.parts {
		if strings.EqualFold(p.Role, "judge") {
			return p.AgentName
		}
	}
	return ""
}

func (o *debate) debaters(judge string) []Participant {
	out := make([]Participant, 0, len(o.parts))
	for _, p := range o.parts {
		if p.AgentName == judge {
			continue
		}
		out = append(out, p)
	}
	return out
}

func buildDebatePrompt(input, snapshot string, round int) string {
	var sb strings.Builder
	sb.WriteString("Debate round " + strconv.Itoa(round) + ".\n")
	if snapshot != "" {
		sb.WriteString(snapshot + "\n\n")
	}
	sb.WriteString("Argue your position on: " + input)
	return sb.String()
}

func buildJudgePrompt(input, snapshot string) string {
	return snapshot + "\n\nAs the judge, weigh the arguments above and give a final verdict on: " + input
}
