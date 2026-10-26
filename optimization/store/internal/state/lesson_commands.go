package state

import (
	"crypto/sha256"
	"encoding/hex"
	"strings"

	"github.com/xraph/cortex/optimization"
	"github.com/xraph/cortex/optimization/internal/canonical"
)

// lessonCommand checks the shared scope-local command namespace before mutable
// delivery state. A receipt remains valid after later cycles replace LastRetry.
func (tx *transaction) lessonCommand(key string, action optimization.Action, request any) (digest string, repeated bool, err error) {
	if key == "" {
		return "", false, nil
	}
	decoded, err := hex.DecodeString(strings.TrimPrefix(key, "operator/"))
	if !strings.HasPrefix(key, "operator/") || err != nil || len(decoded) != sha256.Size {
		return "", false, optimization.ErrInvalidSpec
	}
	payload, err := canonical.Marshal(struct {
		Action  optimization.Action `json:"action"`
		Request any                 `json:"request"`
	}{action, request})
	if err != nil {
		return "", false, err
	}
	sum := sha256.Sum256(payload)
	digest = hex.EncodeToString(sum[:])
	if prior, ok := tx.data.LessonCommands[key]; ok {
		if prior != digest {
			return "", false, optimization.ErrConflict
		}
		return digest, true, nil
	}
	return digest, false, nil
}
func (tx *transaction) rememberLessonCommand(key, digest string) {
	if key == "" {
		return
	}
	if tx.data.LessonCommands == nil {
		tx.data.LessonCommands = make(map[string]string)
	}
	tx.data.LessonCommands[key] = digest
}
