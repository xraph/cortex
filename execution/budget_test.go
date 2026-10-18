package execution_test

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/xraph/cortex/execution"
)

func TestUnknownUsageCannotResetBudget(t *testing.T) {
	budget := execution.Budget{MaxSteps: 3, MaxTokens: 10}
	usage := execution.Usage{}
	usage.ChargeTokens(6, budget)
	encoded, err := json.Marshal(usage)
	if err != nil {
		t.Fatal(err)
	}
	var restored execution.Usage
	if err = json.Unmarshal(encoded, &restored); err != nil {
		t.Fatal(err)
	}
	restored.ChargeTokens(0, budget)
	if restored.Tokens != 10 || !restored.Estimated {
		t.Fatalf("unknown usage reset budget: %+v", restored)
	}
	if got := budget.Check(restored, time.Now()); got != execution.LimitTokens {
		t.Fatalf("limit=%q", got)
	}
}

func TestBudgetBoundaries(t *testing.T) {
	now := time.Now()
	tests := []struct {
		b    execution.Budget
		u    execution.Usage
		want execution.Limit
	}{
		{execution.Budget{MaxSteps: 2}, execution.Usage{Steps: 2}, execution.LimitSteps},
		{execution.Budget{MaxTokens: 10}, execution.Usage{Tokens: 10}, execution.LimitTokens},
		{execution.Budget{Deadline: now}, execution.Usage{}, execution.LimitDeadline},
		{execution.Budget{MaxSteps: 2, MaxTokens: 10}, execution.Usage{Steps: 1, Tokens: 9}, ""},
	}
	for _, tc := range tests {
		if got := tc.b.Check(tc.u, now); got != tc.want {
			t.Errorf("got %q want %q", got, tc.want)
		}
	}
}
