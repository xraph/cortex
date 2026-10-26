// Package rules screens bounded text against explicit host-owned regular
// expressions. Rules cover only their configured patterns. They do not establish
// semantic safety, factual correctness or resistance to arbitrary obfuscation.
package rules

import (
	"context"
	"regexp"
	"regexp/syntax"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/xraph/cortex/safety"
)

// Rule blocks or redacts a pattern. Replacement is literal text, never a regexp
// expansion. Findings name the rule and never contain matched input.
type Rule struct {
	ID          string          `json:"id"`
	Pattern     string          `json:"pattern"`
	Decision    safety.Decision `json:"decision"`
	Replacement string          `json:"replacement,omitempty"`
}

// Config is trusted host policy. Both directions use every rule. MaxBytes is
// required and bounds input and transformed output to at most one MiB. Revision
// identifies the immutable installed policy; changing rules requires a new one.
type Config struct {
	Revision string `json:"revision"`
	MaxBytes int    `json:"max_bytes"`
	Rules    []Rule `json:"rules"`
}

type compiledRule struct {
	rule    Rule
	pattern *regexp.Regexp
}

// Scanner is immutable after construction and safe for concurrent scans.
// Blocking rules take precedence over redaction regardless of policy order.
type Scanner struct {
	revision string
	maxBytes int
	rules    []compiledRule
}

// New compiles a nonempty, finite policy. Unsupported decisions, duplicate IDs,
// empty matches and malformed expressions prevent installation.
func New(c Config) (*Scanner, error) {
	if strings.TrimSpace(c.Revision) == "" || len(c.Revision) > 256 || !utf8.ValidString(c.Revision) || c.MaxBytes < 1 || c.MaxBytes > 1<<20 || len(c.Rules) < 1 || len(c.Rules) > 128 {
		return nil, safety.ErrUnavailable
	}
	s := &Scanner{revision: c.Revision, maxBytes: c.MaxBytes}
	seen := make(map[string]bool, len(c.Rules))
	for _, r := range c.Rules {
		if strings.TrimSpace(r.ID) == "" || len(r.ID) > 128 || !utf8.ValidString(r.ID) || seen[r.ID] || r.Pattern == "" || len(r.Pattern) > 4096 || len(r.Replacement) > 4096 || !utf8.ValidString(r.Pattern) || !utf8.ValidString(r.Replacement) {
			return nil, safety.ErrUnavailable
		}
		if r.Decision != safety.DecisionBlock && r.Decision != safety.DecisionRedact {
			return nil, safety.ErrUnavailable
		}
		if r.Decision == safety.DecisionBlock && r.Replacement != "" {
			return nil, safety.ErrUnavailable
		}
		parsed, parseErr := syntax.Parse(r.Pattern, syntax.Perl)
		if parseErr != nil || zeroWidthPossible(parsed) {
			return nil, safety.ErrUnavailable
		}
		p, err := regexp.Compile(r.Pattern)
		if err != nil || p.MatchString("") {
			return nil, safety.ErrUnavailable
		}
		seen[r.ID] = true
		s.rules = append(s.rules, compiledRule{rule: r, pattern: p})
	}
	return s, nil
}

// ScanInput checks complete input without mutating the request.
func (s *Scanner) ScanInput(ctx context.Context, r *safety.ScanRequest) (*safety.ScanResult, error) {
	return s.scan(ctx, r)
}

// ScanOutput checks complete output. Use buffered publication for this scanner.
func (s *Scanner) ScanOutput(ctx context.Context, r *safety.ScanRequest) (*safety.ScanResult, error) {
	return s.scan(ctx, r)
}

func (s *Scanner) scan(ctx context.Context, r *safety.ScanRequest) (*safety.ScanResult, error) {
	started := time.Now()
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if s == nil || len(s.rules) == 0 || s.revision == "" || s.maxBytes < 1 || r == nil || !utf8.ValidString(r.Content) {
		return nil, safety.ErrUnavailable
	}
	if len(r.Content) > s.maxBytes {
		return nil, safety.ErrLimit
	}
	result := &safety.ScanResult{Decision: safety.DecisionAllow, ProfileUsed: s.revision}
	text := r.Content
	for _, rule := range s.rules {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if !rule.pattern.MatchString(r.Content) {
			continue
		}
		result.Findings = append(result.Findings, safety.Finding{Layer: "host_rules", Source: rule.rule.ID, Severity: "policy", Message: "configured rule matched"})
		if rule.rule.Decision == safety.DecisionBlock {
			result.Decision = safety.DecisionBlock
			result.Blocked = true
			result.Duration = time.Since(started)
			return result, nil
		}
	}
	for _, rule := range s.rules {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if rule.rule.Decision != safety.DecisionRedact {
			continue
		}
		// Build with a hard output limit before allocation can grow with match count.
		matches := rule.pattern.FindAllStringIndex(text, -1)
		if len(matches) == 0 {
			continue
		}
		var b strings.Builder
		offset := 0
		for _, m := range matches {
			if err := ctx.Err(); err != nil {
				return nil, err
			}
			if b.Len()+m[0]-offset+len(rule.rule.Replacement) > s.maxBytes {
				return nil, safety.ErrLimit
			}
			b.WriteString(text[offset:m[0]])
			b.WriteString(rule.rule.Replacement)
			offset = m[1]
		}
		if b.Len()+len(text)-offset > s.maxBytes {
			return nil, safety.ErrLimit
		}
		b.WriteString(text[offset:])
		text = b.String()
		result.Decision = safety.DecisionRedact
	}
	// A replacement can join text into a new blocked pattern.
	for _, rule := range s.rules {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if rule.pattern.MatchString(text) {
			result.Decision = safety.DecisionBlock
			result.Blocked = true
			result.Duration = time.Since(started)
			return result, nil
		}
	}
	if result.Decision == safety.DecisionRedact {
		result.Redacted = text
	}
	result.Duration = time.Since(started)
	return result, nil
}

// zeroWidthPossible conservatively rejects expressions whose minimum consumed
// width is zero, including anchors that match only inside nonempty input.
func zeroWidthPossible(r *syntax.Regexp) bool {
	switch r.Op {
	case syntax.OpNoMatch:
		return false
	case syntax.OpLiteral:
		return len(r.Rune) == 0
	case syntax.OpCharClass, syntax.OpAnyCharNotNL, syntax.OpAnyChar:
		return false
	case syntax.OpCapture, syntax.OpPlus:
		return zeroWidthPossible(r.Sub[0])
	case syntax.OpRepeat:
		return r.Min == 0 || zeroWidthPossible(r.Sub[0])
	case syntax.OpConcat:
		for _, sub := range r.Sub {
			if !zeroWidthPossible(sub) {
				return false
			}
		}
		return true
	case syntax.OpAlternate:
		for _, sub := range r.Sub {
			if zeroWidthPossible(sub) {
				return true
			}
		}
		return false
	default:
		return true
	}
}
