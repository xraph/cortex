//go:build linux || darwin

package hostfs

import (
	"context"
	"errors"
	"fmt"
	"io"

	"github.com/xraph/cortex/evidence"
	"github.com/xraph/cortex/integrations/sandbox/internal/verifiedio"
)

// Pin identifies an owner-controlled regular file. Expected values come from
// approved deployment configuration, never from the file being checked.
type Pin struct {
	Path   string `json:"path"`
	SHA256 string `json:"sha256"`
	Bytes  int64  `json:"bytes"`
	Mode   uint32 `json:"mode"`
}

// VerifyPin checks every path component, ownership, exact mode, length and hash.
// The trusted owner must keep verified bytes unchanged until their final use.
func VerifyPin(ctx context.Context, pin Pin, owner uint32) error {
	if !evidence.ValidDigest(pin.SHA256) || pin.Bytes <= 0 || pin.Bytes > 2<<30 {
		return fmt.Errorf("cortex: invalid host file pin")
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	file, size, err := openOwnedRegular(pin.Path, owner, pin.Mode)
	if err != nil {
		return err
	}
	if size != pin.Bytes {
		return errors.Join(fmt.Errorf("cortex: unsafe or mismatched pinned host file"), file.Close())
	}
	return verifiedio.Read(ctx, file, io.Discard, pin.Bytes, pin.Bytes, pin.SHA256)
}
