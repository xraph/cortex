//go:build linux || darwin

package hostfs

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"

	"github.com/xraph/cortex/evidence"
	"github.com/xraph/cortex/integrations/sandbox/internal/verifiedio"
)

// Pin identifies an owner-controlled regular file. The digest and mode come
// from approved host policy. Bytes is an exact length, or a bounded size hint
// when the independently trusted digest authenticates the complete content.
type Pin struct {
	Path   string `json:"path"`
	SHA256 string `json:"sha256"`
	Bytes  int64  `json:"bytes"`
	Mode   uint32 `json:"mode"`
}

// VerifyPin checks every path component, ownership, exact mode, length and hash.
// The trusted owner must keep verified bytes unchanged until their final use.
func VerifyPin(ctx context.Context, pin Pin, owner uint32) error {
	file, err := openPin(ctx, pin, owner)
	if err != nil {
		return err
	}
	return verifiedio.Read(ctx, file, io.Discard, pin.Bytes, pin.Bytes, pin.SHA256)
}

// ReadPin returns an independently verified snapshot of at most 64 MiB.
// Callers must bound concurrent snapshots. Use a responsive local filesystem;
// context cancellation cannot forcibly interrupt a stalled kernel file call.
func ReadPin(ctx context.Context, pin Pin, owner uint32) ([]byte, error) {
	if pin.Bytes < 0 || pin.Bytes > 64<<20 {
		return nil, fmt.Errorf("cortex: invalid host snapshot size")
	}
	file, err := openPin(ctx, pin, owner)
	if err != nil {
		return nil, err
	}
	buffer := bytes.NewBuffer(make([]byte, 0, int(pin.Bytes)))
	if readErr := verifiedio.Read(ctx, file, buffer, pin.Bytes, pin.Bytes, pin.SHA256); readErr != nil {
		return nil, readErr
	}
	return buffer.Bytes(), nil
}

func openPin(ctx context.Context, pin Pin, owner uint32) (*os.File, error) {
	if !evidence.ValidDigest(pin.SHA256) || pin.Bytes < 0 || pin.Bytes > 2<<30 {
		return nil, fmt.Errorf("cortex: invalid host file pin")
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	file, size, err := openOwnedRegular(pin.Path, owner, pin.Mode)
	if err != nil {
		return nil, err
	}
	if size != pin.Bytes {
		return nil, errors.Join(fmt.Errorf("cortex: unsafe or mismatched pinned host file"), file.Close())
	}
	return file, nil
}
