package a2aremote

import (
	"context"
	"errors"
	"testing"

	"github.com/xraph/cortex/a2a"
	"github.com/xraph/cortex/engine"
)

func TestAttachRefusesAnEngineWithoutMessaging(t *testing.T) {
	eng, err := engine.New()
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	_, err = Attach(eng, AttachOptions{Resolver: okResolver()})
	if !errors.Is(err, ErrNoMessaging) {
		t.Fatalf("err = %v, want ErrNoMessaging", err)
	}
}

// A service with no resolver would answer anyone, so building one is
// refused rather than defaulted.
func TestAttachRefusesAMissingResolver(t *testing.T) {
	if _, err := Attach(nil, AttachOptions{}); err == nil {
		t.Fatal("attaching to no engine must fail")
	}
}

func TestEngineGatewaySatisfiesTheSeam(t *testing.T) {
	var gw Gateway = EngineGateway(nil)
	if gw == nil {
		t.Fatal("the adapter must satisfy Gateway")
	}
	_ = context.Background()
	_ = a2a.Address{}
}
