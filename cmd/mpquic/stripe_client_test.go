package main

import (
	"context"
	"reflect"
	"testing"
)

// TestNewStripeClientConn_LastRxPtrWiredBeforeRecv is a documentary test that
// pins the constructor signature to ensure the fast-failover lastRx pointer
// is passed in (and therefore wired into the struct literal) BEFORE any
// recv goroutine is spawned. The race detector (`go test -race`) is the
// primary line of defence; this test guarantees the parameter cannot be
// removed silently.
func TestNewStripeClientConn_LastRxPtrWiredBeforeRecv(t *testing.T) {
	fn := reflect.TypeOf(newStripeClientConn)
	if fn.Kind() != reflect.Func {
		t.Fatalf("newStripeClientConn is not a function: %v", fn.Kind())
	}

	wantIn := []reflect.Type{
		reflect.TypeOf((*context.Context)(nil)).Elem(),
		reflect.TypeOf((*Config)(nil)),
		reflect.TypeOf(MultipathPathConfig{}),
		reflect.TypeOf((*stripeKeyMaterial)(nil)),
		reflect.TypeOf((*int64)(nil)), // <-- lastRxNsPtr (combo A+E)
		reflect.TypeOf((*Logger)(nil)),
	}
	if got := fn.NumIn(); got != len(wantIn) {
		t.Fatalf("newStripeClientConn arity = %d, want %d", got, len(wantIn))
	}
	for i, wt := range wantIn {
		if got := fn.In(i); got != wt {
			t.Errorf("newStripeClientConn arg %d = %v, want %v", i, got, wt)
		}
	}

	// Spot-check that the *int64 parameter sits at index 4 (just before
	// logger), matching the call sites in client.go.
	if fn.In(4) != reflect.TypeOf((*int64)(nil)) {
		t.Errorf("lastRxNsPtr expected at arg index 4, got %v", fn.In(4))
	}
}
