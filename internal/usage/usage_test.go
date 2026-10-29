package usage

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

func write(t *testing.T, path string, lines ...string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(strings.Join(lines, "\n")+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
}

// Trimmed real transcript lines, one session per harness.
func fixture(t *testing.T, home string) {
	write(t, filepath.Join(home, ".claude", "projects", "-home-u", "s1.jsonl"),
		`{"type":"assistant","timestamp":"2026-10-01T10:00:00Z","message":{"role":"assistant","content":[{"type":"tool_use","name":"Skill","input":{"skill":"commiting"}}]}}`,
		`{"type":"assistant","timestamp":"2026-10-02T10:00:00Z","message":{"role":"assistant","content":[{"type":"tool_use","name":"Skill","input":{"skill":"commiting"}}]}}`,
		`{"type":"user","timestamp":"2026-10-01T11:00:00Z","message":{"role":"user","content":"<command-message>dataviz</command-message>\n<command-name>/dataviz</command-name>"}}`,
		// The skill listing names every skill; it is not a use.
		`{"type":"attachment","timestamp":"2026-10-01T09:00:00Z","attachment":{"type":"skill_listing","content":"- commiting: \"Skill\" listing"}}`,
	)
	write(t, filepath.Join(home, ".codex", "sessions", "2026", "10", "01", "rollout-a.jsonl"),
		// The instructions list every skill's path; only tool calls count.
		`{"timestamp":"2026-10-01T10:00:00Z","type":"session_meta","payload":{"base_instructions":{"text":"- x: (file: /h/.agents/skills/unused/SKILL.md)"}}}`,
		`{"timestamp":"2026-10-01T10:01:00Z","type":"response_item","payload":{"type":"custom_tool_call","name":"exec","input":"cat /h/.agents/skills/playwright-cli/SKILL.md; cat .agents/skills/website-inspection/SKILL.md"}}`,
		`{"timestamp":"2026-10-01T10:02:00Z","type":"response_item","payload":{"type":"function_call_output","output":"/h/.agents/skills/listed/SKILL.md"}}`,
		`{"timestamp":"2026-10-01T10:03:00Z","type":"response_item","payload":{"type":"message","role":"user","content":[{"type":"input_text","text":"<skill>\n<name>fix-all-repo-checks</name>\n<path>/h/.agents/skills/fix-all-repo-checks/SKILL.md</path>"}]}}`,
	)
	write(t, filepath.Join(home, ".pi", "agent", "sessions", "--home-u--", "2026-10-01_p.jsonl"),
		`{"type":"message","timestamp":"2026-10-01T10:00:00Z","message":{"role":"assistant","content":[{"type":"thinking","thinking":"maybe read skills/thought/SKILL.md"},{"type":"toolCall","name":"read","arguments":{"path":"/h/.agents/skills/plane-workflow/SKILL.md"}}]}}`,
		`{"type":"message","timestamp":"2026-10-01T10:05:00Z","message":{"role":"user","content":[{"type":"text","text":"<skill name=\"docs-audit\" location=\"/h/.agents/skills/docs-audit/SKILL.md\">"}]}}`,
	)
}

func key(u Use) string {
	manual := ""
	if u.Manual {
		manual = " manual"
	}
	return u.Harness + " " + u.Skill + " " + u.Day + manual
}

func TestScan(t *testing.T) {
	home := t.TempDir()
	t.Setenv("TZ", "UTC")
	fixture(t, home)
	cachePath := filepath.Join(t.TempDir(), "usage.json")

	want := []string{
		"claude commiting 2026-10-01", // once per session, dated by the first use
		"claude dataviz 2026-10-01 manual",
		"codex fix-all-repo-checks 2026-10-01 manual",
		"codex playwright-cli 2026-10-01",
		"codex website-inspection 2026-10-01",
		"pi docs-audit 2026-10-01 manual",
		"pi plane-workflow 2026-10-01",
	}
	check := func() {
		t.Helper()
		uses, err := Scan(home, cachePath)
		if err != nil {
			t.Fatal(err)
		}
		var got []string
		for _, u := range uses {
			got = append(got, key(u))
		}
		slices.Sort(got)
		if !slices.Equal(got, want) {
			t.Errorf("got\n%s\nwant\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
		}
	}
	check()

	// Codex archives a session by moving it; Claude Code deletes old ones. Both keep
	// their usage through the cache.
	codex := filepath.Join(home, ".codex")
	archived := filepath.Join(codex, "archived_sessions", "rollout-a.jsonl")
	if err := os.MkdirAll(filepath.Dir(archived), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(filepath.Join(codex, "sessions", "2026", "10", "01", "rollout-a.jsonl"), archived); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(home, ".claude", "projects", "-home-u", "s1.jsonl")); err != nil {
		t.Fatal(err)
	}
	check()
}

func TestEachLineHandlesLongLines(t *testing.T) {
	long := strings.Repeat("x", 3<<20)
	var got []int
	if err := eachLine(strings.NewReader("a\n"+long+"\nb"), func(l []byte) { got = append(got, len(strings.TrimSuffix(string(l), "\n"))) }); err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(got, []int{1, len(long), 1}) {
		t.Errorf("got %v", got)
	}
}
