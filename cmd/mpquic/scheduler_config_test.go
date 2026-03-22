package main

import (
	"testing"
)

// ─── pathPolicyScore tests ────────────────────────────────────────────────

func TestPathPolicyScore_Priority(t *testing.T) {
	p := &multipathPathState{
		cfg:              MultipathPathConfig{Priority: 10, Weight: 1},
		consecutiveFails: 0,
	}
	score := pathPolicyScore("priority", p)
	if score != 10000 {
		t.Errorf("score = %d, want 10000 (priority=10 × 1000)", score)
	}
}

func TestPathPolicyScore_Priority_WithFails(t *testing.T) {
	p := &multipathPathState{
		cfg:              MultipathPathConfig{Priority: 10, Weight: 1},
		consecutiveFails: 3,
	}
	score := pathPolicyScore("priority", p)
	// 10*1000 + 3*100 = 10300
	if score != 10300 {
		t.Errorf("score = %d, want 10300", score)
	}
}

func TestPathPolicyScore_Priority_WeightBonus(t *testing.T) {
	p := &multipathPathState{
		cfg:              MultipathPathConfig{Priority: 10, Weight: 3},
		consecutiveFails: 0,
	}
	score := pathPolicyScore("priority", p)
	// 10*1000 + 0 - (3-1)*10 = 10000 - 20 = 9980
	if score != 9980 {
		t.Errorf("score = %d, want 9980", score)
	}
}

func TestPathPolicyScore_Failover(t *testing.T) {
	p := &multipathPathState{
		cfg:              MultipathPathConfig{Priority: 5, Weight: 10},
		consecutiveFails: 0,
	}
	score := pathPolicyScore("failover", p)
	// failover: base + failPenalty only (no weight bonus)
	// 5*1000 + 0 = 5000
	if score != 5000 {
		t.Errorf("score = %d, want 5000 (failover ignores weight)", score)
	}
}

func TestPathPolicyScore_Balanced(t *testing.T) {
	p := &multipathPathState{
		cfg:              MultipathPathConfig{Priority: 10, Weight: 5},
		consecutiveFails: 0,
	}
	score := pathPolicyScore("balanced", p)
	// balanced: 10*1000 - (5-1)*120 = 10000 - 480 = 9520
	if score != 9520 {
		t.Errorf("score = %d, want 9520", score)
	}
}

func TestPathPolicyScore_FailoverPrefersPriority(t *testing.T) {
	primary := &multipathPathState{
		cfg:              MultipathPathConfig{Priority: 1, Weight: 1},
		consecutiveFails: 0,
	}
	backup := &multipathPathState{
		cfg:              MultipathPathConfig{Priority: 100, Weight: 10},
		consecutiveFails: 0,
	}
	if pathPolicyScore("failover", primary) >= pathPolicyScore("failover", backup) {
		t.Error("failover should prefer lower priority (primary over backup)")
	}
}

func TestPathPolicyScore_BalancedFavorsWeight(t *testing.T) {
	heavy := &multipathPathState{
		cfg:              MultipathPathConfig{Priority: 10, Weight: 5},
		consecutiveFails: 0,
	}
	light := &multipathPathState{
		cfg:              MultipathPathConfig{Priority: 10, Weight: 1},
		consecutiveFails: 0,
	}
	if pathPolicyScore("balanced", heavy) >= pathPolicyScore("balanced", light) {
		t.Error("balanced should favor higher weight (lower score)")
	}
}

// ─── normalizeDataplaneConfig tests ────────────────────────────────────────

func TestNormalizeDataplaneConfig_Defaults(t *testing.T) {
	dp := DataplaneConfig{}
	normalizeDataplaneConfig(&dp, "")

	if dp.DefaultClass != "default" {
		t.Errorf("defaultClass = %q, want 'default'", dp.DefaultClass)
	}
	if len(dp.Classes) == 0 {
		t.Fatal("classes should have at least the default class")
	}
	defClass := dp.Classes["default"]
	if defClass.SchedulerPolicy != "priority" {
		t.Errorf("default class policy = %q, want 'priority'", defClass.SchedulerPolicy)
	}
}

func TestNormalizeDataplaneConfig_TrimAndLowercase(t *testing.T) {
	dp := DataplaneConfig{
		DefaultClass: "  Critical  ",
		Classes: map[string]DataplaneClassPolicy{
			"  Critical  ": {SchedulerPolicy: "  Failover  "},
		},
	}
	normalizeDataplaneConfig(&dp, "priority")

	if dp.DefaultClass != "critical" {
		t.Errorf("defaultClass = %q, want 'critical'", dp.DefaultClass)
	}
	if _, ok := dp.Classes["critical"]; !ok {
		t.Error("class name should be normalized to lowercase")
	}
	if dp.Classes["critical"].SchedulerPolicy != "failover" {
		t.Errorf("policy = %q, want 'failover'", dp.Classes["critical"].SchedulerPolicy)
	}
}

func TestNormalizeDataplaneConfig_DuplicateCopiesClamp(t *testing.T) {
	dp := DataplaneConfig{
		Classes: map[string]DataplaneClassPolicy{
			"dup-low": {
				SchedulerPolicy: "priority",
				Duplicate:       true,
				DuplicateCopies: 0, // below minimum
			},
			"dup-high": {
				SchedulerPolicy: "priority",
				Duplicate:       true,
				DuplicateCopies: 10, // above maximum
			},
		},
	}
	normalizeDataplaneConfig(&dp, "priority")

	if dp.Classes["dup-low"].DuplicateCopies != 2 {
		t.Errorf("dup-low copies = %d, want 2 (minimum)", dp.Classes["dup-low"].DuplicateCopies)
	}
	if dp.Classes["dup-high"].DuplicateCopies != 3 {
		t.Errorf("dup-high copies = %d, want 3 (clamped)", dp.Classes["dup-high"].DuplicateCopies)
	}
}

func TestNormalizeDataplaneConfig_EmptyClassNameSkipped(t *testing.T) {
	dp := DataplaneConfig{
		DefaultClass: "default",
		Classes: map[string]DataplaneClassPolicy{
			"default": {SchedulerPolicy: "priority"},
			"   ":     {SchedulerPolicy: "failover"}, // blank → should be removed
		},
	}
	normalizeDataplaneConfig(&dp, "priority")

	if len(dp.Classes) != 1 {
		t.Errorf("classes count = %d, want 1 (blank class removed)", len(dp.Classes))
	}
}

func TestNormalizeDataplaneConfig_Classifiers(t *testing.T) {
	dp := DataplaneConfig{
		DefaultClass: "default",
		Classes: map[string]DataplaneClassPolicy{
			"default":  {SchedulerPolicy: "priority"},
			"critical": {SchedulerPolicy: "failover"},
		},
		Classifiers: []DataplaneClassifierRule{
			{Name: " VoIP ", ClassName: " Critical ", Protocol: " UDP "},
		},
	}
	normalizeDataplaneConfig(&dp, "priority")

	if dp.Classifiers[0].Name != "VoIP" {
		t.Errorf("classifier name = %q, want 'VoIP'", dp.Classifiers[0].Name)
	}
	if dp.Classifiers[0].ClassName != "critical" {
		t.Errorf("classifier class = %q, want 'critical'", dp.Classifiers[0].ClassName)
	}
	if dp.Classifiers[0].Protocol != "udp" {
		t.Errorf("classifier protocol = %q, want 'udp'", dp.Classifiers[0].Protocol)
	}
}

// ─── validateDataplaneConfig tests ──────────────────────────────────────

func TestValidateDataplaneConfig_Valid(t *testing.T) {
	dp := DataplaneConfig{
		DefaultClass: "default",
		Classes: map[string]DataplaneClassPolicy{
			"default": {SchedulerPolicy: "priority"},
		},
	}
	if err := validateDataplaneConfig(dp, nil); err != nil {
		t.Errorf("unexpected error: %v", err)
	}
}

func TestValidateDataplaneConfig_EmptyClasses(t *testing.T) {
	dp := DataplaneConfig{
		DefaultClass: "default",
		Classes:      map[string]DataplaneClassPolicy{},
	}
	if err := validateDataplaneConfig(dp, nil); err == nil {
		t.Error("expected error for empty classes")
	}
}

func TestValidateDataplaneConfig_DefaultClassMissing(t *testing.T) {
	dp := DataplaneConfig{
		DefaultClass: "nonexistent",
		Classes: map[string]DataplaneClassPolicy{
			"default": {SchedulerPolicy: "priority"},
		},
	}
	err := validateDataplaneConfig(dp, nil)
	if err == nil {
		t.Error("expected error when default_class not in classes")
	}
}

func TestValidateDataplaneConfig_InvalidPolicy(t *testing.T) {
	dp := DataplaneConfig{
		DefaultClass: "default",
		Classes: map[string]DataplaneClassPolicy{
			"default": {SchedulerPolicy: "round-robin"}, // invalid
		},
	}
	err := validateDataplaneConfig(dp, nil)
	if err == nil {
		t.Error("expected error for invalid scheduler_policy")
	}
}

func TestValidateDataplaneConfig_UnknownPreferredPath(t *testing.T) {
	dp := DataplaneConfig{
		DefaultClass: "default",
		Classes: map[string]DataplaneClassPolicy{
			"default": {
				SchedulerPolicy: "priority",
				PreferredPaths:  []string{"ghost"},
			},
		},
	}
	paths := []MultipathPathConfig{
		{Name: "wan1"},
	}
	err := validateDataplaneConfig(dp, paths)
	if err == nil {
		t.Error("expected error for unknown preferred_paths")
	}
}

func TestValidateDataplaneConfig_ValidPreferredPath(t *testing.T) {
	dp := DataplaneConfig{
		DefaultClass: "default",
		Classes: map[string]DataplaneClassPolicy{
			"default": {
				SchedulerPolicy: "priority",
				PreferredPaths:  []string{"wan1"},
			},
		},
	}
	paths := []MultipathPathConfig{
		{Name: "wan1"},
		{Name: "wan2"},
	}
	err := validateDataplaneConfig(dp, paths)
	if err != nil {
		t.Errorf("unexpected error: %v", err)
	}
}

func TestValidateDataplaneConfig_ClassifierUnknownClass(t *testing.T) {
	dp := DataplaneConfig{
		DefaultClass: "default",
		Classes: map[string]DataplaneClassPolicy{
			"default": {SchedulerPolicy: "priority"},
		},
		Classifiers: []DataplaneClassifierRule{
			{ClassName: "nonexistent", Protocol: "tcp"},
		},
	}
	err := validateDataplaneConfig(dp, nil)
	if err == nil {
		t.Error("expected error for classifier referencing unknown class")
	}
}

func TestValidateDataplaneConfig_InvalidProtocol(t *testing.T) {
	dp := DataplaneConfig{
		DefaultClass: "default",
		Classes: map[string]DataplaneClassPolicy{
			"default": {SchedulerPolicy: "priority"},
		},
		Classifiers: []DataplaneClassifierRule{
			{ClassName: "default", Protocol: "sctp"},
		},
	}
	err := validateDataplaneConfig(dp, nil)
	if err == nil {
		t.Error("expected error for invalid protocol")
	}
}

func TestValidateDataplaneConfig_InvalidDSCP(t *testing.T) {
	dp := DataplaneConfig{
		DefaultClass: "default",
		Classes: map[string]DataplaneClassPolicy{
			"default": {SchedulerPolicy: "priority"},
		},
		Classifiers: []DataplaneClassifierRule{
			{ClassName: "default", DSCP: []int{64}}, // max valid is 63
		},
	}
	err := validateDataplaneConfig(dp, nil)
	if err == nil {
		t.Error("expected error for DSCP > 63")
	}
}

func TestValidateDataplaneConfig_ValidClassifier(t *testing.T) {
	dp := DataplaneConfig{
		DefaultClass: "default",
		Classes: map[string]DataplaneClassPolicy{
			"default":  {SchedulerPolicy: "priority"},
			"critical": {SchedulerPolicy: "failover"},
		},
		Classifiers: []DataplaneClassifierRule{
			{
				Name:      "voip",
				ClassName: "critical",
				Protocol:  "udp",
				DstPorts:  []string{"5060-5061"},
				DSCP:      []int{46},
			},
		},
	}
	err := validateDataplaneConfig(dp, nil)
	if err != nil {
		t.Errorf("unexpected error: %v", err)
	}
}

// ─── mergeDataplaneConfig tests ────────────────────────────────────────────

func TestMergeDataplaneConfig_OverrideDefaultClass(t *testing.T) {
	base := DataplaneConfig{
		DefaultClass: "default",
		Classes: map[string]DataplaneClassPolicy{
			"default": {SchedulerPolicy: "priority"},
		},
	}
	over := DataplaneConfig{
		DefaultClass: "critical",
		Classes: map[string]DataplaneClassPolicy{
			"critical": {SchedulerPolicy: "failover"},
		},
	}
	result := mergeDataplaneConfig(base, over)

	if result.DefaultClass != "critical" {
		t.Errorf("defaultClass = %q, want 'critical'", result.DefaultClass)
	}
	if _, ok := result.Classes["critical"]; !ok {
		t.Error("merged config should have 'critical' class from override")
	}
	if _, ok := result.Classes["default"]; !ok {
		t.Error("merged config should retain 'default' class from base")
	}
}

func TestMergeDataplaneConfig_OverrideClassifiers(t *testing.T) {
	base := DataplaneConfig{
		DefaultClass: "default",
		Classifiers:  []DataplaneClassifierRule{{Name: "old"}},
	}
	over := DataplaneConfig{
		Classifiers: []DataplaneClassifierRule{{Name: "new1"}, {Name: "new2"}},
	}
	result := mergeDataplaneConfig(base, over)
	if len(result.Classifiers) != 2 {
		t.Errorf("classifiers count = %d, want 2", len(result.Classifiers))
	}
	if result.Classifiers[0].Name != "new1" {
		t.Error("override classifiers should replace base classifiers entirely")
	}
}

func TestMergeDataplaneConfig_EmptyOverride(t *testing.T) {
	base := DataplaneConfig{
		DefaultClass: "default",
		Classes: map[string]DataplaneClassPolicy{
			"default": {SchedulerPolicy: "priority"},
		},
	}
	over := DataplaneConfig{}
	result := mergeDataplaneConfig(base, over)
	if result.DefaultClass != "default" {
		t.Error("empty override should preserve base default_class")
	}
}
