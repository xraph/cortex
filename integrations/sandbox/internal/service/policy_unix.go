//go:build linux || darwin

package service

import (
	"context"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/xraph/cortex"
	"github.com/xraph/cortex/integrations/sandbox/internal/hostfs"
)

func readPolicyDocument(ctx context.Context, name string, owner uint32, destination any) error {
	body, err := hostfs.ReadOwned(ctx, name, owner, 0o600, 1<<20)
	if err != nil {
		return err
	}
	return decodeCanonical(body, destination)
}
func validPolicyPrincipal(principal string) bool {
	return strings.TrimSpace(principal) != "" && len(principal) <= 512 && utf8.ValidString(principal) && !strings.ContainsRune(principal, 0)
}
func validPolicySubject(principal string, scope cortex.Scope) bool {
	return validPolicyPrincipal(principal) && cortex.ValidateScope(scope) == nil
}
func validPolicyInterval(start, end time.Time) bool { return !start.IsZero() && end.After(start) }
func policyIntervalWithin(start, end, parentStart, parentEnd time.Time) bool {
	return validPolicyInterval(start, end) && !start.Before(parentStart) && !end.After(parentEnd)
}
