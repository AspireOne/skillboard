package lint

import (
	"errors"
	"strings"
	"testing"
)

// fixIssue applies the fix of the one issue whose message contains substr.
func fixIssue(t *testing.T, in Input, substr string) (Input, error) {
	t.Helper()
	for _, i := range Skill(in).Issues {
		if strings.Contains(i.Message, substr) {
			if i.Fix == nil {
				t.Fatalf("issue %q has no fix", i.Message)
			}
			return ApplyFix(in, i.Message)
		}
	}
	t.Fatalf("no issue containing %q in:\n%s", substr, messages(Skill(in)))
	return in, nil
}

func TestFixes(t *testing.T) {
	const fm = "name: demo\ndescription: Does X. Use when Y.\n"
	tests := []struct {
		name, content, openAI string
		issue                 string // substring of the issue to fix
		wantContent           string // "" means unchanged
		wantOpenAI            string // "" means unchanged
	}{
		{
			name:        "byte-order mark",
			content:     "\uFEFF---\n" + fm + "---\n",
			issue:       "byte-order mark",
			wantContent: "---\n" + fm + "---\n",
		},
		{
			name:        "final newline",
			content:     "---\n" + fm + "---\nBody",
			issue:       "doesn't end with a newline",
			wantContent: "---\n" + fm + "---\nBody\n",
		},
		{
			name:        "fence whitespace",
			content:     "--- \n" + fm + "---\t\n",
			issue:       "--- fence",
			wantContent: "---\n" + fm + "---\n",
		},
		{
			name:        "unquoted colon",
			content:     "---\nname: demo\ndescription: Does X: well. Use when Y.\n---\n",
			issue:       "not valid YAML",
			wantContent: "---\nname: demo\ndescription: 'Does X: well. Use when Y.'\n---\n",
		},
		{
			name:        "misspelled field",
			content:     "---\n" + fm + "allowed_tools: Read\n---\n",
			issue:       "Unknown field allowed_tools",
			wantContent: "---\n" + fm + "allowed-tools: Read\n---\n",
		},
		{
			name:        "missing name",
			content:     "---\ndescription: Does X. Use when Y.\n---\n",
			issue:       "no name",
			wantContent: "---\nname: demo\ndescription: Does X. Use when Y.\n---\n",
		},
		{
			name:        "name differs from the directory",
			content:     "---\nname: other # old\ndescription: Does X. Use when Y.\n---\n",
			issue:       "does not match the directory",
			wantContent: "---\nname: demo # old\ndescription: Does X. Use when Y.\n---\n",
		},
		{
			name:        "description cut by a comment",
			content:     "---\nname: demo\ndescription: Use when Y #1 matters\n---\n",
			issue:       "ends in a YAML comment",
			wantContent: "---\nname: demo\ndescription: 'Use when Y #1 matters'\n---\n",
		},
		{
			name:        "when_to_use folds into the description",
			content:     "---\nname: demo\ndescription: Does X.\nwhen_to_use: Use when Y.\nlicense: MIT\n---\n",
			issue:       "when_to_use is read only",
			wantContent: "---\nname: demo\ndescription: Does X. Use when Y.\nlicense: MIT\n---\n",
		},
		{
			name:        "YAML 1.1 boolean",
			content:     "---\n" + fm + "user-invocable: yes\n---\n",
			issue:       "user-invocable must be exactly",
			wantContent: "---\n" + fm + "user-invocable: true\n---\n",
		},
		{
			name:        "metadata number",
			content:     "---\n" + fm + "metadata:\n  version: 1.0\n---\n",
			issue:       "metadata.version should be a string",
			wantContent: "---\n" + fm + "metadata:\n  version: \"1.0\"\n---\n",
		},
		{
			name:        "allowed-tools list",
			content:     "---\n" + fm + "allowed-tools:\n  - Read\n  - Grep\n\n# tools above\nlicense: MIT\n---\n",
			issue:       "allowed-tools is a list",
			wantContent: "---\n" + fm + "allowed-tools: Read Grep\n\n# tools above\nlicense: MIT\n---\n",
		},
		{
			name:       "manual-only creates openai.yaml",
			content:    "---\n" + fm + "disable-model-invocation: true\n---\n",
			issue:      "Codex still invokes it",
			wantOpenAI: "policy:\n  allow_implicit_invocation: false\n",
		},
		{
			name:       "manual-only adds the policy section",
			content:    "---\n" + fm + "disable-model-invocation: true\n---\n",
			openAI:     "interface:\n  display_name: Demo\n",
			issue:      "Codex still invokes it",
			wantOpenAI: "interface:\n  display_name: Demo\npolicy:\n  allow_implicit_invocation: false\n",
		},
		{
			name:       "manual-only adds the key to the policy",
			content:    "---\n" + fm + "disable-model-invocation: true\n---\n",
			openAI:     "policy:\n  products:\n    - chatgpt\n",
			issue:      "Codex still invokes it",
			wantOpenAI: "policy:\n  allow_implicit_invocation: false\n  products:\n    - chatgpt\n",
		},
		{
			name:       "manual-only flips the policy",
			content:    "---\n" + fm + "disable-model-invocation: true\n---\n",
			openAI:     "policy:\n  allow_implicit_invocation: true\n",
			issue:      "Codex still invokes it",
			wantOpenAI: "policy:\n  allow_implicit_invocation: false\n",
		},
		{
			name:        "Codex manual-only makes the skill manual-only",
			content:     "---\n" + fm + "---\n",
			openAI:      codexManualOnly,
			issue:       "Codex won't invoke it",
			wantContent: "---\n" + fm + "disable-model-invocation: true\n---\n",
		},
		{
			name:       "redundant implicit invocation",
			content:    "---\n" + fm + "---\n",
			openAI:     "interface:\n  display_name: Demo\npolicy:\n  allow_implicit_invocation: true\n",
			issue:      "Codex's default",
			wantOpenAI: "interface:\n  display_name: Demo\n",
		},
		{
			name:       "redundant implicit invocation empties openai.yaml",
			content:    "---\n" + fm + "---\n",
			openAI:     "policy:\n  allow_implicit_invocation: true\n",
			issue:      "Codex's default",
			wantOpenAI: "-", // removed
		},
		{
			name:        "field order",
			content:     "---\n# Shown in /skills\ndescription: Does X. Use when Y.\nlicense: MIT\n\n# The id\nname: demo\n---\n",
			issue:       "Put name first",
			wantContent: "---\n# Shown in /skills\n\n# The id\nname: demo\ndescription: Does X. Use when Y.\nlicense: MIT\n---\n",
		},
		{
			name:        "trailing whitespace",
			content:     "---\nname: demo  \ndescription: Does X. Use when Y.\t\n---\nBody stays  \n",
			issue:       "trailing whitespace on line 2, 3",
			wantContent: "---\nname: demo\ndescription: Does X. Use when Y.\n---\nBody stays  \n",
		},
		{
			name:       "tab indentation",
			content:    "---\n" + fm + "---\n",
			openAI:     "policy:\n  products: [chatgpt,\n\tcodex]\n",
			issue:      "tab indentation",
			wantOpenAI: "policy:\n  products: [chatgpt,\n  codex]\n",
		},
		{
			name:        "unnecessary quotes",
			content:     "---\nname: \"demo\"\ndescription: 'Does X. Use when Y.'\nmetadata:\n  \"k\": \"v # not a comment\"\n  tags:\n    - 'a'\n---\n",
			issue:       "unnecessary quotes",
			wantContent: "---\nname: demo\ndescription: Does X. Use when Y.\nmetadata:\n  k: \"v # not a comment\"\n  tags:\n    - a\n---\n",
		},
		{
			name:        "indentation",
			content:     "---\n" + fm + "metadata:\n    a: x\n    b:\n          c: y\n---\n",
			issue:       "indent the keys",
			wantContent: "---\n" + fm + "metadata:\n  a: x\n  b:\n    c: y\n---\n",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			in := Input{DirName: "demo", Content: tt.content, HasSkillFile: true, Outside: onDisk}
			if tt.openAI != "" {
				in.OpenAIYAML = []byte(tt.openAI)
			}
			out, err := fixIssue(t, in, tt.issue)
			if err != nil {
				t.Fatal(err)
			}
			wantContent, wantOpenAI := tt.wantContent, tt.wantOpenAI
			if wantContent == "" {
				wantContent = tt.content
			}
			switch wantOpenAI {
			case "":
				wantOpenAI = tt.openAI
			case "-":
				if out.OpenAIYAML != nil {
					t.Errorf("openai.yaml should be removed, got %q", out.OpenAIYAML)
				}
				wantOpenAI = ""
			}
			if out.Content != wantContent {
				t.Errorf("SKILL.md:\n got %q\nwant %q", out.Content, wantContent)
			}
			if string(out.OpenAIYAML) != wantOpenAI {
				t.Errorf("openai.yaml:\n got %q\nwant %q", out.OpenAIYAML, wantOpenAI)
			}
		})
	}
}

// Edits that can't be done safely as text are not offered.
func TestNoFix(t *testing.T) {
	tests := map[string]string{
		"boolean that means nothing": "---\nname: demo\ndescription: Use when Y.\nuser-invocable: maybe\n---\n",
		"directory name is invalid":  "---\nname: Demo\ndescription: Use when Y.\n---\n",
		"multi-line plain value":     "---\nname: demo\ndescription: Use when Y.\nmetadata:\n  v: 1.0\n    2.0\n---\n",
		"tool name with a space":     "---\nname: demo\ndescription: Use when Y.\nallowed-tools:\n  - Bash(git status)\n---\n",
		"duplicate keys":             "---\nname: demo\ndescription: Does X.\ndescription: Use when Y.\nwhen_to_use: Z\n---\n",
	}
	for name, content := range tests {
		t.Run(name, func(t *testing.T) {
			dir := "demo"
			if name == "directory name is invalid" {
				dir = "Demo"
			}
			for _, i := range Skill(Input{DirName: dir, Content: content, HasSkillFile: true, Outside: onDisk}).Issues {
				if i.Fix != nil {
					t.Errorf("unexpected fix %q for %q", i.Fix.Label, i.Message)
				}
			}
		})
	}
}

func TestApplyFixRefuses(t *testing.T) {
	in := Input{DirName: "demo", HasSkillFile: true, Outside: onDisk,
		Content: "---\nname: demo\ndescription: Use when Y.\n---\nBody"}
	if _, err := ApplyFix(in, "a message the linter never reported"); !errors.Is(err, ErrNotFixed) {
		t.Errorf("stale message: got %v", err)
	}

	// Folding pushes the description over the limit, which is a new error.
	in.Content = "---\nname: demo\ndescription: Use when Y.\nwhen_to_use: " + strings.Repeat("x", 1100) + "\n---\n"
	if _, err := fixIssue(t, in, "when_to_use is read only"); !errors.Is(err, ErrNotFixed) || !strings.Contains(err.Error(), "new error") {
		t.Errorf("new error: got %v", err)
	}
}

func TestFixScriptMode(t *testing.T) {
	in := Input{DirName: "demo", HasSkillFile: true, Outside: onDisk,
		Content: "---\nname: demo\ndescription: Use when Y.\n---\nRun [it](scripts/run.py).\n",
		Files:   []string{"SKILL.md", "scripts/run.py", "scripts/other.py"},
		Scripts: []Script{{Path: "scripts/run.py", Shebang: true}, {Path: "scripts/other.py", Shebang: true}}}
	out, err := fixIssue(t, in, "scripts/run.py starts with #!")
	if err != nil {
		t.Fatal(err)
	}
	if !out.Scripts[0].Executable || out.Scripts[1].Executable || in.Scripts[0].Executable {
		t.Errorf("only run.py should become executable, without touching the input: got %+v, input %+v", out.Scripts, in.Scripts)
	}
	if out.Content != in.Content {
		t.Error("a mode fix must not edit SKILL.md")
	}
}

// Regressions from review: each of these used to damage content or mis-target an edit.
func TestFixReviewRegressions(t *testing.T) {
	const fm = "name: demo\ndescription: Use when Y.\n"
	in := func(content, openAI string) Input {
		i := Input{DirName: "demo", Content: content, HasSkillFile: true, Outside: onDisk}
		if openAI != "" {
			i.OpenAIYAML = []byte(openAI)
		}
		return i
	}
	offered := func(i Input, substr string) bool {
		for _, issue := range Skill(i).Issues {
			if strings.Contains(issue.Message, substr) {
				return issue.Fix != nil
			}
		}
		return false
	}

	if offered(in("---\n"+fm+"---\n", "policy:\n  {allow_implicit_invocation: true, products: [chatgpt]}\n"), "Codex's default") {
		t.Error("flow-style policy: Remove would drop the other keys")
	}
	block := in("---\nname: demo\ndescription: |\n  Use when Y.  \n  \tcode\n---\n", "")
	if got := messages(Skill(block)); strings.Contains(got, "trailing whitespace") || strings.Contains(got, "tab indentation") {
		t.Errorf("block scalar content is not style:\n%s", got)
	}
	out, err := fixIssue(t, in("---\n"+fm+"metadata:\n  \"čšž\": \"x\"\n---\n", ""), "unnecessary quotes")
	if err != nil || !strings.Contains(out.Content, "  čšž: x\n") {
		t.Errorf("unicode columns: %v %q", err, out.Content)
	}
	out, err = fixIssue(t, in("---\r\n"+strings.ReplaceAll(fm, "\n", "\r\n")+"user-invocable: yes\r\n---\r\n", ""), "user-invocable must be")
	if err != nil || strings.Contains(strings.ReplaceAll(out.Content, "\r\n", ""), "\n") {
		t.Errorf("CRLF must survive: %v %q", err, out.Content)
	}
	if offered(in("---\nname: demo\ndescription: Does X. # keep me\nwhen_to_use: Use when Y.\n---\n", ""), "when_to_use is read only") {
		t.Error("fold would drop a comment")
	}
	if offered(in("---\nlicense: MIT\ndescription: Use when Y.\n---\n", ""), "Put name first") {
		t.Error("reorder can't resolve a missing name")
	}
}
