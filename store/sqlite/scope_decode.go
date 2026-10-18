package sqlite

import (
	"encoding/json"

	"github.com/xraph/cortex"
)

func decodeStoredScope(canon, l0, l1, l2, extra string) (cortex.Scope, error) {
	var overflow map[string]string
	if err := json.Unmarshal([]byte(extra), &overflow); err != nil {
		return cortex.Scope{}, cortex.ErrInvalidScope
	}
	return cortex.DecodeStoredScope(canon, [3]string{l0, l1, l2}, len(overflow) > 0)
}
