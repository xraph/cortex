package a2a

import (
	"fmt"
	"strings"
	"time"
)

// RenderInput turns an envelope into the text a recipient's run receives.
// It names the sender and the speech act, because an agent that cannot tell
// a request from a proposal cannot answer either one properly.
func RenderInput(e *Envelope) string {
	var sb strings.Builder
	fmt.Fprintf(&sb, "Message from %s (%s)", e.Sender, e.Performative)
	if e.Ontology != "" {
		fmt.Fprintf(&sb, " [ontology: %s]", e.Ontology)
	}
	if e.Protocol != "" {
		fmt.Fprintf(&sb, " [protocol: %s]", e.Protocol)
	}
	sb.WriteString("\n\n")
	sb.WriteString(e.Content)
	if e.ReplyBy != nil {
		fmt.Fprintf(&sb, "\n\nReply by %s.", e.ReplyBy.Format(time.RFC3339))
	}
	return sb.String()
}
