package memory

import (
	"encoding/json"
	"strings"
	"unicode/utf8"

	"github.com/xraph/cortex"
)

// DecodeStoredMessage rejects malformed persisted conversation rows. Callers
// must fail the complete history read rather than silently skip a bad row.
// Errors never include the stored content or parser diagnostics.
func DecodeStoredMessage(data []byte) (Message, error) {
	var message Message
	if !utf8.Valid(data) || json.Unmarshal(data, &message) != nil || strings.TrimSpace(message.Role) == "" {
		return Message{}, cortex.ErrConversationUnavailable
	}
	return message, nil
}
