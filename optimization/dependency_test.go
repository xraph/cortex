package optimization

import (
	"context"
	"os/exec"
	"strings"
	"testing"
	"time"
)

func TestDependencyBoundaries(t *testing.T) {
	for _, check := range []struct {
		pkg       string
		forbidden []string
	}{
		{".", []string{"github.com/xraph/forge", "github.com/xraph/sentinel", "github.com/xraph/fabriq"}},
		{"github.com/xraph/cortex/engine", []string{"github.com/xraph/cortex/optimization"}},
	} {
		t.Run(check.pkg, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
			defer cancel()
			output, err := exec.CommandContext(ctx, "go", "list", "-deps", check.pkg).CombinedOutput()
			if err != nil {
				t.Fatalf("dependency inspection: %v\n%s", err, output)
			}
			for _, dependency := range strings.Fields(string(output)) {
				for _, forbidden := range check.forbidden {
					if dependency == forbidden || strings.HasPrefix(dependency, forbidden+"/") {
						t.Fatalf("%s imports forbidden dependency %s", check.pkg, dependency)
					}
				}
			}
		})
	}
}
