package cognitive

import (
	"encoding/json"
	"reflect"
	"strconv"
	"unicode/utf8"

	"github.com/xraph/cortex/evidence"
	"github.com/xraph/cortex/llm"
)

// ContextRecord preserves the initial selection without storing conversation text
// in run metadata. Original history belongs in private verification evidence.
type ContextRecord struct {
	SchemaVersion  int              `json:"schema_version"`
	Policy         ContextPolicy    `json:"policy"`
	Input          ContextInput     `json:"input"`
	Selection      ContextSelection `json:"selection"`
	OriginalDigest string           `json:"original_digest"`
	SelectedDigest string           `json:"selected_digest"`
	FixedDigest    string           `json:"fixed_digest"`
}

// Validate checks the bounded deterministic selection and its content identities.
// It does not establish token-counter accuracy or source availability.
func (r ContextRecord) Validate() error {
	if r.SchemaVersion != 1 || !evidence.ValidDigest(r.OriginalDigest) || !evidence.ValidDigest(r.SelectedDigest) || !evidence.ValidDigest(r.FixedDigest) {
		return ErrInvalidContext
	}
	selection, err := r.Policy.Select(r.Input)
	if err != nil || !reflect.DeepEqual(selection, r.Selection) {
		return ErrInvalidContext
	}
	for i, item := range r.Input.Items {
		key := strconv.Itoa(i)
		if item.Key != key || item.Kind != ContextHistory || item.Origin != ContextConversation || item.Source.Reference != "history:"+key || len(item.Sources) != 0 || item.Uncertainty != "" {
			return ErrInvalidContext
		}
	}
	data, err := json.Marshal(r)
	if err != nil || len(data) > 4<<20 {
		return ErrInvalidContext
	}
	return nil
}

// HistoryGroups returns detached turns, starting a new group at each user
// message. Conversation data cannot inject system, developer or tool roles.
// The engine's saved history is a text projection; tool traces live separately.
func HistoryGroups(messages []llm.Message) ([][]llm.Message, error) {
	if _, err := ContextHistoryDigest(messages); err != nil {
		return nil, err
	}
	groups := make([][]llm.Message, 0)
	for _, message := range messages {
		if len(groups) == 0 || message.Role == "user" {
			groups = append(groups, nil)
		}
		groups[len(groups)-1] = append(groups[len(groups)-1], message)
	}
	return groups, nil
}

// ContextHistoryDigest binds up to 4096 text messages in a one-MiB projection.
// Empty and absent histories have the same canonical identity.
func ContextHistoryDigest(messages []llm.Message) (string, error) {
	if len(messages) > 4096 {
		return "", ErrInvalidContext
	}
	for _, message := range messages {
		if (message.Role != "user" && message.Role != "assistant") || !utf8.ValidString(message.Content) || len(message.ToolCalls) != 0 || message.ToolCallID != "" {
			return "", ErrInvalidContext
		}
	}
	if len(messages) == 0 {
		return evidence.Digest([]byte("[]")), nil
	}
	data, err := json.Marshal(messages)
	if err != nil || len(data) > 1<<20 {
		return "", ErrInvalidContext
	}
	return evidence.Digest(data), nil
}

// ValidateHistory proves that selected history consists exactly of the retained
// original groups, with their recorded digests and order.
func (r ContextRecord) ValidateHistory(original, selected []llm.Message) error {
	if r.Validate() != nil {
		return ErrInvalidContext
	}
	originalDigest, err := ContextHistoryDigest(original)
	if err != nil || originalDigest != r.OriginalDigest {
		return ErrInvalidContext
	}
	selectedDigest, err := ContextHistoryDigest(selected)
	if err != nil || selectedDigest != r.SelectedDigest {
		return ErrInvalidContext
	}
	groups, err := HistoryGroups(original)
	if err != nil || len(groups) != len(r.Input.Items) {
		return ErrInvalidContext
	}
	keep := make(map[string]bool, len(r.Selection.Retained))
	for _, item := range r.Selection.Retained {
		keep[item.Key] = true
	}
	var expected []llm.Message
	for i, group := range groups {
		digest, digestErr := ContextHistoryDigest(group)
		if digestErr != nil || digest != r.Input.Items[i].Source.Digest {
			return ErrInvalidContext
		}
		if keep[strconv.Itoa(i)] {
			expected = append(expected, group...)
		}
	}
	expectedDigest, err := ContextHistoryDigest(expected)
	if err != nil || expectedDigest != selectedDigest {
		return ErrInvalidContext
	}
	return nil
}
