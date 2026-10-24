package cortex_test

import (
	"context"
	"encoding/hex"
	"errors"
	"reflect"
	"testing"

	"github.com/xraph/cortex"
)

func TestInvalidScopeNeverRetainsParentAuthority(t *testing.T) {
	parent := cortex.WithScope(context.Background(), cortex.Scope{Levels: []cortex.Level{{Key: "workspace", Value: "parent"}}})
	invalid := []cortex.Scope{
		{Levels: []cortex.Level{{Key: "workspace", Value: ""}}},
		{Levels: []cortex.Level{{Key: "workspace", Value: "a"}, {Key: "workspace", Value: "b"}}},
		{Levels: []cortex.Level{{Key: "workspace", Value: "a/project=b"}}},
		{Levels: []cortex.Level{{Key: "workspace", Value: " "}}},
		{Levels: []cortex.Level{{Key: "a", Value: "1"}, {Key: "b", Value: "2"}, {Key: "c", Value: "3"}, {Key: "d", Value: "4"}}},
	}
	for _, scope := range invalid {
		if got := cortex.ScopeFromContext(cortex.WithScope(parent, scope)); !got.IsZero() {
			t.Errorf("invalid scope retained authority: %q", got.Canonical())
		}
	}
}

func TestContextScopeIsDetached(t *testing.T) {
	scope := cortex.Scope{Levels: []cortex.Level{{Key: "workspace", Value: "original"}}}
	ctx := cortex.WithScope(context.Background(), scope)
	scope.Levels[0].Value = "changed"
	first := cortex.ScopeFromContext(ctx)
	if first.Canonical() != "workspace=original" {
		t.Fatal("caller mutated attached scope")
	}
	first.Levels[0].Value = "changed again"
	if cortex.ScopeFromContext(ctx).Canonical() != "workspace=original" {
		t.Fatal("reader mutated attached scope")
	}
}

func TestCanonicalRejectsUnsupportedIdentity(t *testing.T) {
	for _, value := range []string{"a=1/a=2", "a=1=2", "a=1/b=2/c=3/d=4", "a= "} {
		if _, err := cortex.ParseCanonical(value); err == nil {
			t.Errorf("accepted %q", value)
		}
	}
}

func TestScopeRejectsInvalidUTF8BeforeIdentityEncoding(t *testing.T) {
	parent := cortex.WithScope(context.Background(), cortex.Scope{Levels: []cortex.Level{{Key: "workspace", Value: "parent"}}})
	for _, value := range []string{string([]byte{0xff}), string([]byte{0xc0, 0xaf}), string([]byte{0xe2, 0x82}), string([]byte{0xed, 0xa0, 0x80})} {
		for _, field := range []string{"key", "value"} {
			t.Run(field+"/"+hex.EncodeToString([]byte(value)), func(t *testing.T) {
				scope := cortex.Scope{Levels: []cortex.Level{{Key: "workspace", Value: "team"}}}
				if field == "key" {
					scope.Levels[0].Key = value
				} else {
					scope.Levels[0].Value = value
				}
				if err := cortex.ValidateScope(scope); !errors.Is(err, cortex.ErrInvalidScope) {
					t.Fatalf("invalid UTF-8 scope accepted: %v", err)
				}
				ctx := cortex.WithScope(parent, scope)
				if _, err := cortex.RequireScope(ctx); !errors.Is(err, cortex.ErrInvalidScope) {
					t.Fatalf("context accepted invalid identity: %v", err)
				}
				if !cortex.ScopeFromContext(ctx).IsZero() {
					t.Fatal("invalid identity retained parent authority")
				}
				canon := scope.Canonical()
				if _, err := cortex.ParseCanonical(canon); !errors.Is(err, cortex.ErrInvalidScope) {
					t.Fatalf("canonical decoder accepted invalid identity: %v", err)
				}
				if _, err := cortex.DecodeStoredScope(canon, [3]string{canon}, false); !errors.Is(err, cortex.ErrInvalidScope) {
					t.Fatalf("stored decoder accepted invalid identity: %v", err)
				}
			})
		}
	}
}

func TestScopePreservesValidUnicodeWithoutNormalization(t *testing.T) {
	for _, value := range []string{"team�", "café", "café", "研究"} {
		scope := cortex.Scope{Levels: []cortex.Level{{Key: "工作区", Value: value}}}
		if err := cortex.ValidateScope(scope); err != nil {
			t.Fatalf("valid Unicode rejected: %v", err)
		}
		parsed, err := cortex.ParseCanonical(scope.Canonical())
		if err != nil || !reflect.DeepEqual(scope, parsed) {
			t.Fatalf("Unicode identity changed: %v %v", parsed, err)
		}
		decoded, err := cortex.DecodeStoredScope(scope.Canonical(), [3]string{scope.Canonical()}, false)
		if err != nil || !reflect.DeepEqual(scope, decoded) {
			t.Fatalf("stored Unicode identity changed: %v %v", decoded, err)
		}
	}
}
