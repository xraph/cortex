package orchestration

import (
	"errors"
	"strings"
	"testing"

	"github.com/xraph/cortex"
)

func TestDelegationPlanStrictSchema(t *testing.T) {
	workers := []Participant{{AgentName: "w1"}, {AgentName: "w2"}}
	valid := `[{"agent":"w1","task":"one"},{"agent":"w2","task":"two"}]`
	for name, output := range map[string]string{
		"prose":            "plan: " + valid,
		"trailing":         valid + ` []`,
		"null":             `null`,
		"empty":            `[]`,
		"unknown field":    `[{"agent":"w1","task":"one","extra":true},{"agent":"w2","task":"two"}]`,
		"duplicate key":    `[{"agent":"w1","agent":"w1","task":"one"},{"agent":"w2","task":"two"}]`,
		"duplicate worker": `[{"agent":"w1","task":"one"},{"agent":"w1","task":"two"}]`,
		"empty task":       `[{"agent":"w1","task":" "},{"agent":"w2","task":"two"}]`,
		"null task":        `[{"agent":"w1","task":null},{"agent":"w2","task":"two"}]`,
		"oversized task":   `[{"agent":"w1","task":"` + strings.Repeat("x", 64*1024+1) + `"},{"agent":"w2","task":"two"}]`,
		"oversized plan":   strings.Repeat(" ", 256*1024) + valid,
	} {
		t.Run(name, func(t *testing.T) {
			tasks, err := parseDelegations(output, workers)
			var invalid *InvalidDelegationPlanError
			if !errors.Is(err, cortex.ErrInvalidDelegationPlan) || !errors.As(err, &invalid) || tasks != nil {
				t.Fatalf("tasks=%v error=%v", tasks, err)
			}
		})
	}
	tasks, err := parseDelegations(valid, workers)
	if err != nil || len(tasks) != 2 || tasks[0].Task != "one" {
		t.Fatalf("valid plan: %v %v", tasks, err)
	}
}
