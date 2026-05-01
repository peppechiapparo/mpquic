package main

import (
	"testing"
	"time"
)

// TestApplyFastFailoverDefaults validates the four defaults plus the two
// validation clamps in applyFastFailoverDefaults.
func TestApplyFastFailoverDefaults(t *testing.T) {
	t.Run("zero_values_apply_defaults", func(t *testing.T) {
		c := &Config{}
		applyFastFailoverDefaults(c)
		if c.StripeFastKeepaliveInterval != stripeKeepaliveInterval {
			t.Errorf("StripeFastKeepaliveInterval = %v, want %v", c.StripeFastKeepaliveInterval, stripeKeepaliveInterval)
		}
		if c.StripePathDegradedThreshold != stripePathDegradedThreshold {
			t.Errorf("StripePathDegradedThreshold = %v, want %v", c.StripePathDegradedThreshold, stripePathDegradedThreshold)
		}
		if c.StripePathDegradedRecovery != stripePathDegradedRecovery {
			t.Errorf("StripePathDegradedRecovery = %v, want %v", c.StripePathDegradedRecovery, stripePathDegradedRecovery)
		}
		if c.StripeHealthCheckInterval != stripeHealthCheckInterval {
			t.Errorf("StripeHealthCheckInterval = %v, want %v", c.StripeHealthCheckInterval, stripeHealthCheckInterval)
		}
	})

	t.Run("recovery_ge_threshold_clamp", func(t *testing.T) {
		c := &Config{
			StripeFastKeepaliveInterval: 1 * time.Second,
			StripePathDegradedThreshold: 2 * time.Second,
			StripePathDegradedRecovery:  3 * time.Second, // invalid: >= threshold
			StripeHealthCheckInterval:   500 * time.Millisecond,
		}
		applyFastFailoverDefaults(c)
		if c.StripePathDegradedThreshold != stripePathDegradedThreshold {
			t.Errorf("threshold not clamped: got %v, want %v", c.StripePathDegradedThreshold, stripePathDegradedThreshold)
		}
		if c.StripePathDegradedRecovery != stripePathDegradedRecovery {
			t.Errorf("recovery not clamped: got %v, want %v", c.StripePathDegradedRecovery, stripePathDegradedRecovery)
		}
	})

	t.Run("health_gt_recovery_clamp", func(t *testing.T) {
		c := &Config{
			StripeFastKeepaliveInterval: 1 * time.Second,
			StripePathDegradedThreshold: 3 * time.Second,
			StripePathDegradedRecovery:  1 * time.Second,
			StripeHealthCheckInterval:   2 * time.Second, // invalid: > recovery
		}
		applyFastFailoverDefaults(c)
		if c.StripeHealthCheckInterval != stripeHealthCheckInterval {
			t.Errorf("health-check not clamped: got %v, want %v", c.StripeHealthCheckInterval, stripeHealthCheckInterval)
		}
		// Threshold/recovery were valid → must not be reset.
		if c.StripePathDegradedThreshold != 3*time.Second {
			t.Errorf("threshold mutated unexpectedly: got %v", c.StripePathDegradedThreshold)
		}
		if c.StripePathDegradedRecovery != 1*time.Second {
			t.Errorf("recovery mutated unexpectedly: got %v", c.StripePathDegradedRecovery)
		}
	})

	t.Run("valid_values_preserved", func(t *testing.T) {
		c := &Config{
			StripeFastKeepaliveInterval: 750 * time.Millisecond,
			StripePathDegradedThreshold: 4 * time.Second,
			StripePathDegradedRecovery:  2 * time.Second,
			StripeHealthCheckInterval:   1 * time.Second,
		}
		applyFastFailoverDefaults(c)
		if c.StripeFastKeepaliveInterval != 750*time.Millisecond {
			t.Errorf("StripeFastKeepaliveInterval = %v, want 750ms", c.StripeFastKeepaliveInterval)
		}
		if c.StripePathDegradedThreshold != 4*time.Second {
			t.Errorf("StripePathDegradedThreshold = %v, want 4s", c.StripePathDegradedThreshold)
		}
		if c.StripePathDegradedRecovery != 2*time.Second {
			t.Errorf("StripePathDegradedRecovery = %v, want 2s", c.StripePathDegradedRecovery)
		}
		if c.StripeHealthCheckInterval != 1*time.Second {
			t.Errorf("StripeHealthCheckInterval = %v, want 1s", c.StripeHealthCheckInterval)
		}
	})
}
