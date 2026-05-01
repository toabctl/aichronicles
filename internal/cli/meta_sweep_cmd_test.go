package cli

import (
	"testing"
	"time"

	"github.com/toabctl/aichronicles/internal/config"
)

// TestMetaAnalysisSweepOptionsFromConfig_AppliesDefaults pins the
// defaulting rules that used to live in cmd/aichroniclesd/main.go.
// Empty config in → built-in cadences out, with the float64 / int
// guard for SkillRevisionMinRate and SkillRevisionMax (which can't
// use the Duration.Or helper).
func TestMetaAnalysisSweepOptionsFromConfig_AppliesDefaults(t *testing.T) {
	t.Parallel()
	got := MetaAnalysisSweepOptionsFromConfig(config.MetaAnalysis{})

	want := MetaAnalysisSweepOptions{
		ProposeCadence:       DefaultMetaProposeCadence,
		ReflectCadence:       DefaultMetaReflectCadence,
		ChallengeCadence:     DefaultMetaChallengeCadence,
		ReflectWeeklyCadence: DefaultMetaReflectWeeklyCadence,
		SkillRevisionCadence: DefaultMetaSkillRevisionCadence,
		SkillRevisionSince:   DefaultMetaSkillRevisionSince,
		SkillRevisionMinRate: DefaultMetaSkillRevisionMinRate,
		SkillRevisionMax:     DefaultMetaSkillRevisionMaxPerSweep,
	}
	if got.ProposeCadence != want.ProposeCadence {
		t.Errorf("ProposeCadence: got %v, want %v", got.ProposeCadence, want.ProposeCadence)
	}
	if got.ReflectCadence != want.ReflectCadence {
		t.Errorf("ReflectCadence: got %v, want %v", got.ReflectCadence, want.ReflectCadence)
	}
	if got.ChallengeCadence != want.ChallengeCadence {
		t.Errorf("ChallengeCadence: got %v, want %v", got.ChallengeCadence, want.ChallengeCadence)
	}
	if got.ReflectWeeklyCadence != want.ReflectWeeklyCadence {
		t.Errorf("ReflectWeeklyCadence: got %v, want %v", got.ReflectWeeklyCadence, want.ReflectWeeklyCadence)
	}
	if got.SkillRevisionCadence != want.SkillRevisionCadence {
		t.Errorf("SkillRevisionCadence: got %v, want %v", got.SkillRevisionCadence, want.SkillRevisionCadence)
	}
	if got.SkillRevisionSince != want.SkillRevisionSince {
		t.Errorf("SkillRevisionSince: got %v, want %v", got.SkillRevisionSince, want.SkillRevisionSince)
	}
	if got.SkillRevisionMinRate != want.SkillRevisionMinRate {
		t.Errorf("SkillRevisionMinRate: got %v, want %v", got.SkillRevisionMinRate, want.SkillRevisionMinRate)
	}
	if got.SkillRevisionMax != want.SkillRevisionMax {
		t.Errorf("SkillRevisionMax: got %v, want %v", got.SkillRevisionMax, want.SkillRevisionMax)
	}
}

// TestMetaAnalysisSweepOptionsFromConfig_RespectsExplicitValues
// confirms that operator-set values win over defaults, including
// the SkillRevisionMinRate / SkillRevisionMax guards (which only
// fire on <= 0).
func TestMetaAnalysisSweepOptionsFromConfig_RespectsExplicitValues(t *testing.T) {
	t.Parallel()
	cfg := config.MetaAnalysis{
		ProposeCadence:       config.Duration(12 * time.Hour),
		ReflectSkip:          true,
		SkillRevisionMinRate: 0.75,
		SkillRevisionMax:     10,
	}
	got := MetaAnalysisSweepOptionsFromConfig(cfg)

	if got.ProposeCadence != 12*time.Hour {
		t.Errorf("ProposeCadence: got %v, want 12h", got.ProposeCadence)
	}
	if !got.ReflectSkip {
		t.Errorf("ReflectSkip: got false, want true")
	}
	if got.SkillRevisionMinRate != 0.75 {
		t.Errorf("SkillRevisionMinRate: got %v, want 0.75", got.SkillRevisionMinRate)
	}
	if got.SkillRevisionMax != 10 {
		t.Errorf("SkillRevisionMax: got %v, want 10", got.SkillRevisionMax)
	}
}

// TestNewMetaCmd_WiresSweepSubcommand verifies the cobra tree shape.
// We don't run the command (it would require a real LLM); we just
// confirm the command name, the child, and the --db flag are wired,
// so a typo in root.go wiring fails fast in CI rather than at the
// first systemd timer firing.
func TestNewMetaCmd_WiresSweepSubcommand(t *testing.T) {
	t.Parallel()
	root := newMetaCmd()
	if root.Use != "meta" {
		t.Fatalf("root Use: got %q, want %q", root.Use, "meta")
	}
	sweep, _, err := root.Find([]string{"sweep"})
	if err != nil {
		t.Fatalf("find sweep subcommand: %v", err)
	}
	if sweep.Use != "sweep" {
		t.Errorf("sweep Use: got %q, want %q", sweep.Use, "sweep")
	}
	if sweep.Flag("db") == nil {
		t.Errorf("sweep is missing --db flag")
	}
}
