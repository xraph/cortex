// Package verifiedio provides bounded content copying for worker staging.
package verifiedio

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"sync"

	"github.com/xraph/cortex/evidence"
)

// Read copies from a source that must unblock Read when Close is called.
// It closes the source exactly once, including on cancellation, and retains
// close errors. A negative size permits up to maximum bytes; otherwise the
// exact size is required. The digest is a lowercase SHA256 without a prefix.
func Read(ctx context.Context, source io.ReadCloser, destination io.Writer, size, maximum int64, digest string) error {
	var once sync.Once
	var closeErr error
	closeReader := func() { once.Do(func() { closeErr = source.Close() }) }
	stop := context.AfterFunc(ctx, closeReader)
	copyErr := CopyLimit(ctx, source, destination, size, maximum, digest)
	stop()
	closeReader()
	return errors.Join(copyErr, closeErr, ctx.Err())
}

// Copy requires the exact byte count and digest before reporting success.
func Copy(ctx context.Context, source io.Reader, destination io.Writer, size int64, digest string) error {
	return CopyLimit(ctx, source, destination, size, size, digest)
}

// CopyLimit bounds bytes even when the source size is unknown.
func CopyLimit(ctx context.Context, source io.Reader, destination io.Writer, size, maximum int64, digest string) error {
	if maximum < 0 || maximum > 8<<30 || size > maximum || size < -1 {
		return evidence.ErrTooLarge
	}
	reader := io.LimitReader(source, maximum+1)
	hash := sha256.New()
	writer := io.MultiWriter(destination, hash)
	buffer := make([]byte, 64<<10)
	var total int64
	empty := 0
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		n, err := reader.Read(buffer)
		if n > 0 {
			total += int64(n)
			if total > maximum {
				return evidence.ErrDigestMismatch
			}
			written, writeErr := writer.Write(buffer[:n])
			if writeErr != nil {
				return writeErr
			}
			if written != n {
				return io.ErrShortWrite
			}
			empty = 0
		} else {
			empty++
			if empty > 100 {
				return io.ErrNoProgress
			}
		}
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return err
		}
	}
	if (size >= 0 && total != size) || hex.EncodeToString(hash.Sum(nil)) != digest {
		return evidence.ErrDigestMismatch
	}
	return ctx.Err()
}
