package lint

import (
	"fmt"
	"strings"
	"testing"
)

func messages(r Result) string {
	var out []string
	for _, i := range r.Issues {
		out = append(out, string(i.Severity)+": "+i.Message)
	}
	return strings.Join(out, "\n")
}

// onDisk pretends only ../../AGENTS.md exists outside the skill.
func onDisk(rel string) bool { return rel == "../../AGENTS.md" }

const codexManualOnly = "policy:\n  allow_implicit_invocation: false\n"

func TestSkill(t *testing.T) {
	tests := []struct {
		name    string
		content string
		files   []string
		openAI  string
		want    []string // substrings expected in the issues; nil means no issues
	}{
		{
			name:    "clean skill",
			content: "---\nname: demo\ndescription: Does X. Use when Y.\n---\n\nSee [ref](references/a.md).\n",
			files:   []string{"SKILL.md", "references/a.md"},
		},
		{
			name:    "folded YAML description counts parsed text",
			content: "---\nname: demo\ndescription: >-\n  Does X.\n  Use when Y.\n---\n",
		},
		{
			name:    "CRLF line endings parse like LF",
			content: "---\r\nname: demo\r\ndescription: Use when Y.\r\n---\r\n",
		},
		{
			name:    "missing frontmatter",
			content: "# demo\n",
			want:    []string{"no frontmatter"},
		},
		{
			name:    "unquoted colon inside a value",
			content: "---\nname: demo\ndescription: Deploy to AWS: ECS. Use when deploying.\n---\n",
			want:    []string{`quote the description value, since an unquoted ": " inside it is invalid. Codex repairs this`},
		},
		{
			name:    "name mismatch and missing trigger",
			content: "---\nname: other\ndescription: Does X.\n---\n",
			want:    []string{`does not match the directory name "demo"`, "not when to use it"},
		},
		{
			name:    "read-before trigger",
			content: "---\nname: demo\ndescription: Manages X. Read before changing Y.\n---\n",
		},
		{
			name:    "reading is not a trigger",
			content: "---\nname: demo\ndescription: Helps read PDFs for analysis.\n---\n",
			want:    []string{"not when to use it"},
		},
		{
			name:    "invalid name characters",
			content: "---\nname: Demo_Skill\ndescription: Use when Y.\n---\n",
			want:    []string{"must be lowercase letters"},
		},
		{
			name:    "needless and needed quotes",
			content: "---\nname: demo\ndescription: \"Use when Y.\"\nmetadata:\n  version: \"1.0\"\n---\n",
			want:    []string{"SKILL.md: unnecessary quotes around description;"},
		},
		{
			name:    "comment silently truncates description",
			content: "---\nname: demo\ndescription: Use when fixing issue #12 # or similar\n---\n",
			want:    []string{"ends in a YAML comment"},
		},
		{
			name:    "angle brackets in description",
			content: "---\nname: demo\ndescription: Use when given a <branch>.\n---\n",
			want:    []string{"&lt;"},
		},
		{
			name:    "duplicate, unknown and misspelled fields",
			content: "---\nname: demo\ndescription: Use when Y.\ndescription: Use when Z.\nauthor: me\ndisable_model_invocation: true\n---\n",
			want:    []string{"SKILL.md: description defined more than once, so Pi refuses", "Unknown field author", "did you mean disable-model-invocation"},
		},
		{
			name:    "manual-only skill consistent with Codex",
			content: "---\nname: demo\ndescription: Does X.\ndisable-model-invocation: true\n---\n",
			openAI:  codexManualOnly,
		},
		{
			name:    "manual-only skill without Codex policy",
			content: "---\nname: demo\ndescription: Does X.\ndisable-model-invocation: true\n---\n",
			want:    []string{"error: Manual-only in Claude Code and Pi, but Codex still invokes it automatically"},
		},
		{
			name:    "Codex-only manual policy",
			content: "---\nname: demo\ndescription: Use when Y.\n---\n",
			openAI:  codexManualOnly,
			want:    []string{"error: Codex won't invoke it automatically"},
		},
		{
			name:    "non-literal boolean",
			content: "---\nname: demo\ndescription: Does X. Use when Y.\ndisable-model-invocation: yes\n---\n",
			want:    []string{"must be exactly true or false"},
		},
		{
			name:    "Claude Code extensions",
			content: "---\nname: demo\ndescription: Use when Y.\nwhen_to_use: Or Z.\nmodel: opus\n---\nRun ${CLAUDE_SKILL_DIR}/x.sh $ARGUMENTS\n",
			want:    []string{"when_to_use is read only by Claude Code", "model is a Claude Code extension", "${CLAUDE_SKILL_DIR}"},
		},
		{
			name:    "spec field types",
			content: "---\nname: demo\ndescription: Use when Y.\ncompatibility: \"\"\nmetadata:\n  version: 1.0\nallowed-tools: [Read, Bash]\n---\n",
			want:    []string{"compatibility must be a string", "metadata.version should be a string", "allowed-tools is a list"},
		},
		{
			name:    "conventional field order",
			content: "---\ndescription: Use when Y.\nname: demo\n---\n",
			want:    []string{"Put name first and description second"},
		},
		{
			name:    "tabs, trailing spaces and indentation",
			content: "---\nname: demo \ndescription: Use when Y.\nmetadata:\n    owner: me\n---\n",
			want:    []string{"trailing whitespace on line 2", "indent the keys under metadata by 2 spaces"},
		},
		{
			name:    "openai.yaml style and schema",
			content: "---\nname: demo\ndescription: Use when Y.\n---\n",
			openAI:  "interface:\n  display_name: \"Demo\"\n  colour: red\npolicy:\n  allow_implicit_invocation: true\n",
			want: []string{
				"agents/openai.yaml: unnecessary quotes around interface.display_name",
				"agents/openai.yaml: unknown key interface.colour",
				"allow_implicit_invocation: true is Codex's default",
			},
		},
		{
			name:    "nested duplicate keys",
			content: "---\nname: demo\ndescription: Use when Y.\nmetadata:\n  a: x\n  a: y\n---\n",
			want:    []string{"metadata.a defined more than once, so Pi refuses"},
		},
		{
			name:    "flow collections keep their quotes",
			content: "---\nname: demo\ndescription: Use when Y.\nmetadata: {tags: \"x, y\", v: \"a]\"}\n---\n",
		},
		{
			name:    "quotes inside block sequences",
			content: "---\nname: demo\ndescription: Use when Y.\npaths:\n  - \"src\"\n---\n",
			want:    []string{"paths is a Claude Code extension", "SKILL.md: unnecessary quotes around paths[0]"},
		},
		{
			name:    "byte-order mark",
			content: "\ufeff---\nname: demo\ndescription: Use when Y.\n---\n",
			want:    []string{"byte-order mark"},
		},
		{
			name:    "fence with trailing whitespace still parses",
			content: "--- \nname: demo\ndescription: Use when Y.\n---\n",
			want:    []string{"trailing whitespace on a --- fence line"},
		},
		{
			name:    "empty frontmatter",
			content: "---\n---\nBody\n",
			want:    []string{"has no name", "has no description"},
		},
		{
			name:    "tab inside block scalar text is not indentation",
			content: "---\nname: demo\ndescription: |\n  Use when\tY.\n---\n",
		},
		{
			name:    "metadata must be a mapping for Codex",
			content: "---\nname: demo\ndescription: Use when Y.\nmetadata: foo\n---\n",
			want:    []string{"error: metadata must be a map"},
		},
		{
			name:    "invalid boolean skips the Codex comparison",
			content: "---\nname: demo\ndescription: Does X.\ndisable-model-invocation: yes\n---\n",
			openAI:  codexManualOnly,
			want:    []string{"must be exactly true or false", "not when to use it"},
		},
		{
			name:    "Claude Code enum fields",
			content: "---\nname: demo\ndescription: Use when Y.\neffort: extreme\nagent: Explore\n---\n",
			want:    []string{"effort is a Claude Code extension", "agent is a Claude Code extension", "effort must be one of", "agent only takes effect with context: fork"},
		},
		{
			name:    "openai.yaml products and tools are known",
			content: "---\nname: demo\ndescription: Use when Y.\n---\n",
			openAI:  "policy:\n  products:\n    - codex\ndependencies:\n  tools:\n    - type: mcp\n      value: github\n",
		},
		{
			name:    "openai.yaml tool entries are checked",
			content: "---\nname: demo\ndescription: Use when Y.\n---\n",
			openAI:  "dependencies:\n  tools:\n    - type: \"mcp\"\n      bogus: 1\n",
			want: []string{
				"unnecessary quotes around dependencies.tools[0].type",
				"unknown key dependencies.tools[0].bogus",
				"dependencies.tools[0] has no value",
			},
		},
		{
			name:    "openai.yaml type error makes Codex drop the policy",
			content: "---\nname: demo\ndescription: Does X.\ndisable-model-invocation: true\n---\n",
			openAI:  "interface:\n  display_name: 123\npolicy:\n  allow_implicit_invocation: false\n",
			want:    []string{"interface.display_name must be a string, so Codex ignores the whole file", "Codex still invokes it automatically"},
		},
		{
			name:    "openai.yaml duplicate policy",
			content: "---\nname: demo\ndescription: Use when Y.\n---\n",
			openAI:  "policy:\n  allow_implicit_invocation: true\npolicy:\n  allow_implicit_invocation: false\n",
			want:    []string{"policy defined more than once, so Codex ignores the whole file", "true is Codex's default"},
		},
		{
			name:    "openai.yaml field limits",
			content: "---\nname: demo\ndescription: Use when Y.\n---\n",
			openAI:  "interface:\n  brand_color: blue\n",
			want:    []string{"interface.brand_color must be #RRGGBB"},
		},
		{
			name:    "angle-bracket, encoded and reference links",
			content: "---\nname: demo\ndescription: Use when Y.\n---\n[a](<references/my file.md>) [b](references/my%20file.md)\n\n[c]: references/gone.md\n",
			files:   []string{"SKILL.md", "references/my file.md"},
			want:    []string{"Broken link to references/gone.md"},
		},
		{
			name:    "template placeholder and missing final newline",
			content: "---\nname: demo\ndescription: TODO. Use when TODO.\n---\nBody",
			openAI:  "policy:\n  allow_implicit_invocation: false",
			want:    []string{"SKILL.md: the file doesn't end with a newline", "placeholder (TODO)", "agents/openai.yaml: the file doesn't end with a newline", "Codex won't invoke it automatically"},
		},
		{
			name:    "broken link outside code, example link inside code ignored",
			content: "---\nname: demo\ndescription: Use when Y.\n---\n[a](missing.md)\n```\n[b](also-missing.md)\n```\n`[c](x.md)`\n",
			want:    []string{"Broken link to missing.md"},
		},
		{
			name:    "links leaving the skill are checked on disk",
			content: "---\nname: demo\ndescription: Use when Y.\n---\n[a](../../AGENTS.md#rules) [b](../gone.md)\n",
			want:    []string{"Broken link to ../gone.md"},
		},
		{
			name:    "unreferenced reference file",
			content: "---\nname: demo\ndescription: Use when Y.\n---\n",
			files:   []string{"SKILL.md", "references/lost.md", "scripts/run.py"},
			want:    []string{"references/lost.md is never mentioned"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			in := Input{DirName: "demo", Content: tt.content, HasSkillFile: true, Files: tt.files, Outside: onDisk}
			if tt.openAI != "" {
				in.OpenAIYAML = []byte(tt.openAI)
			}
			got := messages(Skill(in))
			if tt.want == nil && got != "" {
				t.Fatalf("want no issues, got:\n%s", got)
			}
			for _, w := range tt.want {
				if !strings.Contains(got, w) {
					t.Errorf("want issue containing %q, got:\n%s", w, got)
				}
			}
			if n := strings.Count(got, "\n") + 1; tt.want != nil && n != len(tt.want) {
				t.Errorf("want %d issue(s), got %d:\n%s", len(tt.want), n, got)
			}
		})
	}
}

func TestSkillMissingFile(t *testing.T) {
	if got := messages(Skill(Input{DirName: "demo", Outside: onDisk})); !strings.Contains(got, "SKILL.md is missing") {
		t.Fatalf("got %q", got)
	}
}

func TestManualOnly(t *testing.T) {
	in := Input{DirName: "demo", HasSkillFile: true, OpenAIYAML: []byte(codexManualOnly), Outside: onDisk,
		Content: "---\nname: demo\ndescription: Does X.\ndisable-model-invocation: true\n---\n"}
	if !Skill(in).ManualOnly {
		t.Fatal("a literal true should mark the skill manual-only")
	}
	in.Content = strings.Replace(in.Content, "true", `"true"`, 1)
	if Skill(in).ManualOnly {
		t.Fatal("a quoted \"true\" is a string, not a boolean")
	}
}

// Values YAML needs quoted (or that YAML 1.1 parsers like serde_yaml would retype)
// must never be reported as needlessly quoted.
func TestQuotesThatMatter(t *testing.T) {
	for _, v := range []string{"yes", "on", "No", "0755", "1.0", "1e3", "~", "null", "true", "a: b", "x #y", "#hash", "- x", "[a]", "{a}", "@x", "`x`", " lead", "trail ", "2001-12-14"} {
		content := fmt.Sprintf("---\nname: demo\ndescription: Use when Y.\nmetadata:\n  k: %q\n---\n", v)
		if got := messages(Skill(Input{DirName: "demo", Content: content, HasSkillFile: true, Outside: onDisk})); strings.Contains(got, "unnecessary quotes") {
			t.Errorf("%q: quotes are needed, got:\n%s", v, got)
		}
	}
}

func TestSkillLineLimit(t *testing.T) {
	lint := func(lines int) string {
		content := "---\nname: demo\ndescription: Use when Y.\n---\n" + strings.Repeat("x\n", lines-4)
		return messages(Skill(Input{DirName: "demo", Content: content, HasSkillFile: true, Outside: onDisk}))
	}
	if got := lint(499); got != "" {
		t.Errorf("499 lines should pass, got:\n%s", got)
	}
	if got := lint(500); !strings.Contains(got, "SKILL.md is 500 lines") {
		t.Errorf("500 lines should warn, got:\n%s", got)
	}
}

func TestScripts(t *testing.T) {
	in := Input{DirName: "demo", Content: "---\nname: demo\ndescription: Use when Y.\n---\n", HasSkillFile: true, Outside: onDisk,
		Scripts: []Script{
			{Path: "scripts/ok.sh", Executable: true, Shebang: true},
			{Path: "scripts/helper.py"}, // run through python, or imported
			{Path: "scripts/run.sh", Shebang: true},
			{Path: "scripts/bare.py", Executable: true},
		}}
	got := messages(Skill(in))
	for _, want := range []string{"warning: scripts/run.sh starts with #! but is not executable", "info: scripts/bare.py is executable but has no #! line"} {
		if !strings.Contains(got, want) {
			t.Errorf("missing %q in:\n%s", want, got)
		}
	}
	for _, clean := range []string{"ok.sh", "helper.py"} {
		if strings.Contains(got, clean) {
			t.Errorf("%s should pass, got:\n%s", clean, got)
		}
	}
}
