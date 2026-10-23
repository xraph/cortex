package contract

import (
	"errors"
	"fmt"
	"testing"

	dc "github.com/xraph/forge/extensions/dashboard/contract"

	"github.com/xraph/cortex"
)

func TestRunRevisionConflictRequiresReload(t *testing.T) {
	mapped := mapError(fmt.Errorf("private details: %w", cortex.ErrRunConflict))
	var conflict *dc.Error
	if !errors.As(mapped, &conflict) || conflict.Code != dc.CodeConflict || conflict.Retryable || conflict.Message != cortex.ErrRunConflict.Error() {
		t.Fatalf("revision conflict must require reconsideration: %+v", mapped)
	}
}
