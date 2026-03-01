package main

import (
	"testing"
)

func TestExpandMultipathPipes_Default(t *testing.T) {
	// No pipes configured → 1:1 mapping, backward compatible
	paths := []MultipathPathConfig{
		{Name: "wan5", BindIP: "10.0.0.1", Priority: 1, Weight: 1},
		{Name: "wan6", BindIP: "10.0.0.2", Priority: 2, Weight: 1},
	}
	cfg := &Config{}
	logger := newLogger("error")

	expanded := expandMultipathPipes(paths, cfg, logger)
	if len(expanded) != 2 {
		t.Fatalf("expected 2 paths, got %d", len(expanded))
	}
	if expanded[0].Name != "wan5" {
		t.Errorf("expected name wan5, got %s", expanded[0].Name)
	}
	if expanded[0].BasePath != "wan5" {
		t.Errorf("expected BasePath wan5, got %s", expanded[0].BasePath)
	}
	if expanded[1].Name != "wan6" {
		t.Errorf("expected name wan6, got %s", expanded[1].Name)
	}
}

func TestExpandMultipathPipes_Explicit(t *testing.T) {
	// pipes: 4 → expand into 4 entries
	paths := []MultipathPathConfig{
		{Name: "wan5", BindIP: "10.0.0.1", Priority: 1, Weight: 1, Pipes: 4},
		{Name: "wan6", BindIP: "10.0.0.2", Priority: 1, Weight: 1, Pipes: 1},
	}
	cfg := &Config{}
	logger := newLogger("error")

	expanded := expandMultipathPipes(paths, cfg, logger)
	if len(expanded) != 5 { // 4 + 1
		t.Fatalf("expected 5 paths, got %d", len(expanded))
	}

	// Check wan5 pipes
	for i := 0; i < 4; i++ {
		name := expanded[i].Name
		expected := "wan5." + string(rune('0'+i))
		if name != expected {
			t.Errorf("pipe %d: expected name %s, got %s", i, expected, name)
		}
		if expanded[i].BasePath != "wan5" {
			t.Errorf("pipe %d: expected BasePath wan5, got %s", i, expanded[i].BasePath)
		}
		if expanded[i].Priority != 1 {
			t.Errorf("pipe %d: expected priority 1, got %d", i, expanded[i].Priority)
		}
		if expanded[i].BindIP != "10.0.0.1" {
			t.Errorf("pipe %d: expected BindIP 10.0.0.1, got %s", i, expanded[i].BindIP)
		}
		if expanded[i].Pipes != 1 {
			t.Errorf("pipe %d: expected Pipes=1 after expansion, got %d", i, expanded[i].Pipes)
		}
	}

	// Check wan6 (single, no expansion)
	if expanded[4].Name != "wan6" {
		t.Errorf("expected wan6, got %s", expanded[4].Name)
	}
	if expanded[4].BasePath != "wan6" {
		t.Errorf("expected BasePath wan6, got %s", expanded[4].BasePath)
	}
}

func TestExpandMultipathPipes_Mixed(t *testing.T) {
	paths := []MultipathPathConfig{
		{Name: "wan4", BindIP: "10.0.0.3", Priority: 1, Pipes: 2},
		{Name: "wan5", BindIP: "10.0.0.1", Priority: 1, Pipes: 3},
		{Name: "wan6", BindIP: "10.0.0.2", Priority: 2},
	}
	cfg := &Config{}
	logger := newLogger("error")

	expanded := expandMultipathPipes(paths, cfg, logger)
	// 2 + 3 + 1 = 6
	if len(expanded) != 6 {
		t.Fatalf("expected 6 paths, got %d", len(expanded))
	}

	expectations := []struct {
		name     string
		basePath string
	}{
		{"wan4.0", "wan4"},
		{"wan4.1", "wan4"},
		{"wan5.0", "wan5"},
		{"wan5.1", "wan5"},
		{"wan5.2", "wan5"},
		{"wan6", "wan6"},
	}

	for i, exp := range expectations {
		if expanded[i].Name != exp.name {
			t.Errorf("idx %d: expected name %s, got %s", i, exp.name, expanded[i].Name)
		}
		if expanded[i].BasePath != exp.basePath {
			t.Errorf("idx %d: expected BasePath %s, got %s", i, exp.basePath, expanded[i].BasePath)
		}
	}
}
