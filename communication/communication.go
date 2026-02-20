// Package communication defines the CommunicationStyle value object — how an agent talks.
package communication

// Style defines how an agent communicates with users.
type Style struct {
	Tone            string  `json:"tone,omitempty"`
	Formality       float64 `json:"formality,omitempty"`
	Verbosity       float64 `json:"verbosity,omitempty"`
	TechnicalLevel  float64 `json:"technical_level,omitempty"`
	EmojiUsage      bool    `json:"emoji_usage,omitempty"`
	PreferredFormat string  `json:"preferred_format,omitempty"`
	AdaptToUser     bool    `json:"adapt_to_user,omitempty"`
}
