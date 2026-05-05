package cli

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/toabctl/aichronicles/internal/store"
	"github.com/toabctl/aichronicles/pkg/llm/prompts"
)

// seedSkillOnDisk writes a minimal SKILL.md under
// <skillsDir>/<name>/SKILL.md so runSkillsEvolve has something to
// read. Returns the dir + path.
func seedSkillOnDisk(t *testing.T, skillsDir, name, body string) string {
	t.Helper()
	dir := filepath.Join(skillsDir, name)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	path := filepath.Join(dir, "SKILL.md")
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	return path
}

// seedSkillLoadAndFailure inserts a skill_load extraction at t,
// followed by a tool_failure 30s later in the same session. Sets
// up the bare-minimum corpus runSkillsEvolve needs to find at
// least one failure example. Each call uses the SESSION id as the
// source_session_id so the (source_agent, source_session_id)
// uniqueness constraint isn't violated when the test seeds
// multiple sessions with the same skill name.
func seedSkillLoadAndFailure(t *testing.T, s *store.Store, sess, skill string, baseTs int64) {
	t.Helper()
	srcSess := "src-" + sess
	if _, err := s.DB().Exec(
		`INSERT INTO sessions(id, source_agent, source_session_id, started_at_ms, ended_at_ms)
		 VALUES (?, 'claude-code', ?, ?, ?)`,
		sess, srcSess, baseTs, baseTs+60*60*1000,
	); err != nil {
		t.Fatalf("seed session: %v", err)
	}

	loadEvent := "load-" + sess
	if _, err := s.DB().Exec(
		`INSERT INTO raw_envelopes(event_id, ingest_seq, source_agent, source_session_id, ts_source_ms, ts_server_ms, envelope_json)
		 VALUES (?, (SELECT COALESCE(MAX(ingest_seq),0)+1 FROM raw_envelopes), 'claude-code', ?, ?, ?, '{}')`,
		loadEvent, srcSess, baseTs, baseTs,
	); err != nil {
		t.Fatalf("load env: %v", err)
	}
	if _, err := s.DB().Exec(
		`INSERT INTO events(event_id, session_id, source_agent, kind, ts_source_ms, content_text)
		 VALUES (?, ?, 'claude-code', 'system_message', ?, 'skill load')`,
		loadEvent, sess, baseTs,
	); err != nil {
		t.Fatalf("load ev: %v", err)
	}
	if _, err := s.DB().Exec(
		`INSERT INTO extractions(event_id, session_id, kind, value)
		 VALUES (?, ?, 'skill_load', ?)`,
		loadEvent, sess, skill,
	); err != nil {
		t.Fatalf("ext: %v", err)
	}

	failEvent := "fail-" + sess
	failTs := baseTs + 30_000
	if _, err := s.DB().Exec(
		`INSERT INTO raw_envelopes(event_id, ingest_seq, source_agent, source_session_id, ts_source_ms, ts_server_ms, envelope_json)
		 VALUES (?, (SELECT COALESCE(MAX(ingest_seq),0)+1 FROM raw_envelopes), 'claude-code', ?, ?, ?, '{}')`,
		failEvent, srcSess, failTs, failTs,
	); err != nil {
		t.Fatalf("fail env: %v", err)
	}
	if _, err := s.DB().Exec(
		`INSERT INTO events(event_id, session_id, source_agent, kind, ts_source_ms, content_text)
		 VALUES (?, ?, 'claude-code', 'tool_failure', ?, 'tool failed: ENOENT /etc/foo')`,
		failEvent, sess, failTs,
	); err != nil {
		t.Fatalf("fail ev: %v", err)
	}
}

func TestSkillsEvolve_WritesV2WhenLLMReturnsRevision(t *testing.T) {
	t.Parallel()
	skillsDir := t.TempDir()
	const skill = "stale-skill"
	originalBody := "---\nname: stale-skill\ndescription: original\n---\n\n## Steps\n- TODO\n"
	skillPath := seedSkillOnDisk(t, skillsDir, skill, originalBody)

	st := openTempCLIStore(t)
	seedSkillLoadAndFailure(t, st, "11111111-1111-1111-1111-111111111111",
		skill, time.Now().Add(-1*time.Hour).UnixMilli())
	seedSkillLoadAndFailure(t, st, "22222222-2222-2222-2222-222222222222",
		skill, time.Now().Add(-2*time.Hour).UnixMilli())

	revisedBody := "---\nname: stale-skill\ndescription: original\n---\n\n## Steps\n- (revised steps based on failures)\n"
	revisionJSON, err := json.Marshal(prompts.SkillRevision{
		RevisedBody:    revisedBody,
		Rationale:      "tightened the trigger to exclude the ENOENT case",
		NoChangeNeeded: false,
	})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	fake := &fakeLLM{toolInput: revisionJSON}

	var out, errOut bytes.Buffer
	if err := runSkillsEvolve(t.Context(), apiForStore(t, st), fakeLLMClientFn(fake),
		skillsEvolveOptions{
			SkillName:   skill,
			SkillsDir:   skillsDir,
			Since:       30 * 24 * time.Hour,
			Window:      10 * time.Minute,
			MaxExamples: 5,
		}, &out, &errOut); err != nil {
		t.Fatalf("runSkillsEvolve: %v", err)
	}

	v2 := skillPath + ".v2"
	body, err := os.ReadFile(v2)
	if err != nil {
		t.Fatalf("read v2: %v", err)
	}
	if string(body) != revisedBody {
		t.Errorf("v2 body mismatch:\n--- got ---\n%s\n--- want ---\n%s", body, revisedBody)
	}
	if !strings.Contains(out.String(), "wrote") {
		t.Errorf("expected wrote-line in output:\n%s", out.String())
	}
	if !strings.Contains(out.String(), "tightened the trigger") {
		t.Errorf("rationale missing from output:\n%s", out.String())
	}

	// Original SKILL.md must be left alone — evolve is non-destructive.
	orig, err := os.ReadFile(skillPath)
	if err != nil {
		t.Fatalf("read original: %v", err)
	}
	if string(orig) != originalBody {
		t.Errorf("original SKILL.md was mutated:\n%s", orig)
	}
}

func TestSkillsEvolve_NoChangeNeededDoesNotWriteV2(t *testing.T) {
	t.Parallel()
	skillsDir := t.TempDir()
	const skill = "fine-skill"
	original := "---\nname: fine-skill\ndescription: ok\n---\n\n## Steps\n- run thing\n"
	skillPath := seedSkillOnDisk(t, skillsDir, skill, original)

	st := openTempCLIStore(t)
	seedSkillLoadAndFailure(t, st, "33333333-3333-3333-3333-333333333333",
		skill, time.Now().Add(-1*time.Hour).UnixMilli())

	revisionJSON, err := json.Marshal(prompts.SkillRevision{
		RevisedBody:    "",
		Rationale:      "failures look unrelated to this skill",
		NoChangeNeeded: true,
	})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	fake := &fakeLLM{toolInput: revisionJSON}

	var out, errOut bytes.Buffer
	if err := runSkillsEvolve(t.Context(), apiForStore(t, st), fakeLLMClientFn(fake),
		skillsEvolveOptions{
			SkillName: skill, SkillsDir: skillsDir,
			Since: 30 * 24 * time.Hour, Window: 10 * time.Minute, MaxExamples: 5,
		}, &out, &errOut); err != nil {
		t.Fatalf("runSkillsEvolve: %v", err)
	}

	if _, err := os.Stat(skillPath + ".v2"); !os.IsNotExist(err) {
		t.Errorf("v2 should NOT exist for no_change_needed=true (stat: %v)", err)
	}
	if !strings.Contains(out.String(), "no revision needed") {
		t.Errorf("expected no-revision-needed message:\n%s", out.String())
	}
}

func TestSkillsEvolve_NoFailureEvidenceShortCircuits(t *testing.T) {
	t.Parallel()
	skillsDir := t.TempDir()
	const skill = "untested-skill"
	seedSkillOnDisk(t, skillsDir, skill, "---\nname: untested-skill\ndescription: x\n---\n")

	st := openTempCLIStore(t)
	// Deliberately no failures seeded.

	// LLM must NOT be called when there's no evidence to ground a
	// revision in.
	fake := &fakeLLM{}

	var out, errOut bytes.Buffer
	if err := runSkillsEvolve(t.Context(), apiForStore(t, st), fakeLLMClientFn(fake),
		skillsEvolveOptions{
			SkillName: skill, SkillsDir: skillsDir,
			Since: 30 * 24 * time.Hour, Window: 10 * time.Minute, MaxExamples: 5,
		}, &out, &errOut); err != nil {
		t.Fatalf("runSkillsEvolve: %v", err)
	}
	if fake.called != 0 {
		t.Errorf("LLM should not be invoked when there's no evidence, got %d calls", fake.called)
	}
	if !strings.Contains(errOut.String(), "no failure evidence") {
		t.Errorf("expected no-evidence message:\n%s", errOut.String())
	}
}
