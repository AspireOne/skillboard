// Package lint checks a skill against the Agent Skills specification
// (https://agentskills.io/specification) and against how the harnesses that read
// ~/.agents actually interpret it: Claude Code, Codex and Pi.
package lint

import (
	"fmt"
	"net/url"
	"path"
	"regexp"
	"slices"
	"strings"
	"unicode/utf16"

	"go.yaml.in/yaml/v3"
)

const (
	// Spec limits.
	maxNameChars          = 64
	maxDescriptionChars   = 1024
	maxCompatibilityChars = 500
	// The spec says to keep SKILL.md under this many lines; detail belongs in references/.
	maxSkillLines = 500
	// Claude Code truncates description + when_to_use to this many characters in its listing.
	maxClaudeListingChars = 1536
	// Codex's agents/openai.yaml limits (codex-rs/skills/src/interface.rs, metadata.rs).
	maxCodexNameChars = 64
	maxCodexTextChars = 1024

	skillFile  = "SKILL.md"
	openAIFile = "agents/openai.yaml"
	// Conventional YAML indentation for nested mappings.
	yamlIndent = 2
	bom        = "\uFEFF"
)

type Severity string

const (
	Error   Severity = "error"
	Warning Severity = "warning"
	Info    Severity = "info"
)

type Issue struct {
	Severity Severity `json:"severity"`
	Message  string   `json:"message"`
	Fix      *Fix     `json:"fix,omitempty"`
}

// Input is everything the linter needs to know about one skill.
type Input struct {
	DirName      string
	Content      string // SKILL.md text
	HasSkillFile bool
	Files        []string // every file in the skill, relative to its directory
	OpenAIYAML   []byte   // agents/openai.yaml, nil when absent
	Scripts      []Script // the files under scripts/
	// Outside reports whether a path that leaves the skill directory exists.
	Outside func(rel string) bool
}

type Script struct {
	Path       string // relative to the skill directory
	Executable bool
	Shebang    bool // starts with #!
}

// Result is the lint outcome plus the parsed values the UI shows next to it.
type Result struct {
	Issues           []Issue `json:"issues"`
	Description      string  `json:"description"`
	DescriptionChars int     `json:"descriptionChars"`
	ManualOnly       bool    `json:"manualOnly"`
}

// Frontmatter fields every harness knows (the spec), and fields only Claude Code reads.
// Anything else is ignored by every harness, which usually means a typo.
var (
	specFields   = []string{"name", "description", "license", "compatibility", "metadata", "allowed-tools"}
	claudeFields = []string{
		"when_to_use", "argument-hint", "arguments", "disable-model-invocation", "user-invocable",
		"disallowed-tools", "model", "effort", "context", "agent", "background", "hooks", "paths", "shell",
	}
	// Pi honors only a literal `true`; Claude Code also accepts yes/on/1. Only true/false
	// mean the same thing everywhere.
	boolFields = []string{"disable-model-invocation", "user-invocable", "background"}
	// Claude Code fields with a closed set of values (code.claude.com/docs/en/skills).
	claudeEnums = map[string][]string{
		"effort":  {"low", "medium", "high", "xhigh", "max"},
		"context": {"fork"},
		"shell":   {"bash", "powershell"},
	}
	// Claude Code fields that only take effect with `context: fork`.
	forkOnlyFields = []string{"agent", "background"}
)

// The agents/openai.yaml schema, from Codex's own structs.
var (
	openAIInterface = map[string]int{ // key -> max length (0: a path, no limit)
		"display_name": maxCodexNameChars, "short_description": maxCodexTextChars,
		"default_prompt": maxCodexTextChars, "brand_color": maxCodexNameChars,
		"icon_small": 0, "icon_large": 0,
	}
	openAIToolKeys = map[string]int{ // key -> max length
		"type": 64, "transport": 64, "value": 1024, "description": 1024, "command": 1024, "url": 1024,
	}
	brandColorPattern = regexp.MustCompile(`^#[0-9A-Fa-f]{6}$`)
)

var (
	namePattern = regexp.MustCompile(`^[a-z0-9]+(-[a-z0-9]+)*$`)
	// Leftovers from the new-skill template or a draft.
	placeholderPattern = regexp.MustCompile(`\b(TODO|TBD|FIXME)\b`)
	// "Use when…" and "Read before…" both tell the agent when to load the skill.
	triggerPattern = regexp.MustCompile(`(?i)\buse\b[^.]{0,30}?\b(when|whenever|for|to|before|after|if)\b|\bread\b[^.]{0,30}?\b(when|whenever|before|if)\b`)
	// YAML 1.1 booleans (Claude Code accepts these) and the literal each one means.
	yamlBools = map[string]string{
		"true": "true", "yes": "true", "y": "true", "on": "true", "1": "true",
		"false": "false", "no": "false", "n": "false", "off": "false", "0": "false",
	}
	// An unquoted value with ": " inside is invalid YAML (Codex repairs it, the others don't).
	unquotedColonPattern = regexp.MustCompile(`(?m)^([A-Za-z_-]+):[ \t]+[^"'|>\s].*:\s`)
	// Substitutions only Claude Code expands; elsewhere they reach the model literally.
	claudeVarPattern = regexp.MustCompile(`\$\{CLAUDE_[A-Z_]+\}|\$ARGUMENTS\b`)
	// Fenced blocks and inline code hold examples, not links SKILL.md relies on.
	codePattern = regexp.MustCompile("(?ms)^[ \t]*(```|~~~).*?^[ \t]*(```|~~~)|`[^`\n]+`")
	// Inline links and images, [text](target) or [text](<target with spaces>), and
	// reference definitions, [label]: target.
	linkPattern    = regexp.MustCompile(`!?\[[^\]]*\]\((<[^>]*>|[^)\s]+)[^)]*\)`)
	refLinkPattern = regexp.MustCompile(`(?m)^ {0,3}\[[^\]]+\]:[ \t]*(<[^>]*>|\S+)`)
	// Directories whose files are meant to be loaded on demand, so SKILL.md should point at them.
	linkedDirs = []string{"references/", "assets/"}
)

// splitFrontmatter returns the YAML between the leading `---` fences and the body after
// them. Like Codex and Pi it accepts trailing whitespace on the fences; exact reports
// whether both fences were exactly "---".
func splitFrontmatter(content string) (yamlText, body string, exact, ok bool) {
	lines := strings.SplitAfter(content, "\n")
	if !isFence(lines[0]) {
		return "", content, false, false
	}
	for i := 1; i < len(lines); i++ {
		if isFence(lines[i]) {
			yamlText = strings.TrimSuffix(strings.Join(lines[1:i], ""), "\n")
			exact = strings.TrimSuffix(lines[0], "\n") == "---" && strings.TrimSuffix(lines[i], "\n") == "---"
			return yamlText, strings.Join(lines[i+1:], ""), exact, true
		}
	}
	return "", content, false, false
}

func isFence(line string) bool { return strings.TrimRight(line, " \t\n") == "---" }

type linter struct {
	in  Input
	res Result
	// manualOnlyValid is false when disable-model-invocation has a non-literal value, so
	// harnesses disagree on it and comparing it with Codex's policy would mislead.
	manualOnlyValid bool

	// What fixes need to edit the files.
	skill, openAI         source
	fmEnd                 int        // index of SKILL.md's closing --- line
	parsed                bool       // the frontmatter is valid YAML
	root                  *yaml.Node // the frontmatter mapping
	dupKeys               bool       // the frontmatter repeats a key, so fields can't be located by name
	policyKey, policyBody *yaml.Node // the policy section of agents/openai.yaml
}

// add records an issue and returns it, so the caller can attach a fix. Attach it right
// away: the pointer goes stale with the next add.
func (l *linter) add(s Severity, format string, args ...any) *Issue {
	l.res.Issues = append(l.res.Issues, Issue{Severity: s, Message: fmt.Sprintf(format, args...)})
	return &l.res.Issues[len(l.res.Issues)-1]
}

func Skill(in Input) Result { return lintSkill(in).res }

func lintSkill(in Input) *linter {
	l := &linter{in: in, manualOnlyValid: true,
		openAI: source{file: openAIFile, lines: strings.Split(strings.ReplaceAll(string(in.OpenAIYAML), "\r\n", "\n"), "\n")}}
	l.run()
	return l
}

func (l *linter) run() {
	if !l.in.HasSkillFile {
		l.add(Error, "SKILL.md is missing")
		return
	}
	l.checkScripts()

	// Pi normalises CRLF before parsing; do the same so Windows-edited files lint correctly.
	content := strings.ReplaceAll(l.in.Content, "\r\n", "\n")
	if rest, found := strings.CutPrefix(content, bom); found {
		// Fixes write the content as read here, so keeping every line drops the BOM.
		l.add(Error, "SKILL.md starts with a byte-order mark, so Codex finds no frontmatter and skips the skill. Save it as UTF-8 without BOM").
			withFix(skillFile, "Remove BOM", func(lines []string) ([]string, error) { return lines, nil })
		content = rest
	}
	l.skill = source{file: skillFile, lines: strings.Split(content, "\n"), first: 1}
	checkFinalNewline(l, skillFile, content)
	if lines := strings.Count(strings.TrimSuffix(content, "\n"), "\n") + 1; lines >= maxSkillLines {
		l.add(Warning, "SKILL.md is %d lines; the spec says to keep it under %d. Move detail into references/", lines, maxSkillLines)
	}

	yamlText, body, exact, ok := splitFrontmatter(content)
	if !ok {
		l.add(Error, "SKILL.md has no frontmatter: the first line must be --- and a closing --- must follow")
		return
	}
	l.fmEnd = slices.IndexFunc(l.skill.lines[1:], isFence) + 1
	if !exact {
		end := l.fmEnd
		l.add(Warning, "%s: trailing whitespace on a --- fence line", skillFile).
			withFix(skillFile, "Trim fences", func(lines []string) ([]string, error) {
				lines[0], lines[end] = "---", "---"
				return lines, nil
			})
	}

	var doc yaml.Node
	if err := yaml.Unmarshal([]byte(yamlText), &doc); err != nil {
		if m := unquotedColonPattern.FindStringSubmatchIndex(yamlText); m != nil {
			key := yamlText[m[2]:m[3]]
			l.add(Error, `Frontmatter is not valid YAML (%v): quote the %s value, since an unquoted ": " inside it is invalid. Codex repairs this, but Claude Code loads the skill without metadata so it never triggers, and Pi skips it`, err, key).
				withFix(skillFile, "Quote value", l.quoteLine(1+strings.Count(yamlText[:m[0]], "\n"), key))
		} else {
			l.add(Error, "Frontmatter is not valid YAML (%v). Claude Code then loads the skill without metadata so it never triggers, and Pi and Codex skip it", err)
		}
		return
	}
	l.parsed = true
	fields := map[string]*yaml.Node{}
	if len(doc.Content) > 0 {
		root := doc.Content[0]
		if root.Kind != yaml.MappingNode {
			l.add(Error, "Frontmatter must be a set of key: value fields")
			return
		}
		l.root = root
		if dups := duplicateKeys(root, ""); len(dups) > 0 {
			l.dupKeys = true
			l.add(Error, "%s: %s defined more than once, so Pi refuses to load the skill", skillFile, strings.Join(dups, ", "))
		}
		fields = l.collectFields(root)
		l.checkOrder(root)
		l.checkStyle(l.skill, yamlText, root)
	}

	l.checkBooleans(fields) // first: manual-only skills are exempt from trigger phrasing
	l.checkName(fields["name"])
	l.checkDescription(fields["description"], fields["when_to_use"])
	l.checkWhenToUse(fields["description"], fields["when_to_use"])
	l.checkOptionalFields(fields)
	l.checkClaudeFields(fields)
	l.checkCodexPolicy(fields["disable-model-invocation"])

	l.checkClaudeVariables(body)
	l.checkLinks(body)
}

// collectFields maps each frontmatter key to its value node, flagging fields no harness reads.
func (l *linter) collectFields(root *yaml.Node) map[string]*yaml.Node {
	fields := map[string]*yaml.Node{}
	for k, v := range pairs(root) {
		key := k.Value
		fields[key] = v
		switch {
		case slices.Contains(specFields, key):
		case slices.Contains(claudeFields, key):
			// Both get their own checks: checkWhenToUse, and against Codex's policy.
			if key != "when_to_use" && key != "disable-model-invocation" {
				l.add(Info, "%s is a Claude Code extension; Codex and Pi ignore it", key)
			}
		default:
			if guess := knownField(key); guess != "" {
				l.add(Error, "Unknown field %s; did you mean %s? Every harness ignores it as written", key, guess).
					withFix(skillFile, "Rename to "+guess, l.renameKey(k, guess), key, guess)
			} else {
				l.add(Warning, "Unknown field %s: no harness reads it", key)
			}
		}
	}
	return fields
}

// renameKey renames a top-level key, unless the new name is taken.
func (l *linter) renameKey(k *yaml.Node, to string) func([]string) ([]string, error) {
	for other := range pairs(l.root) {
		if other.Value == to {
			return nil
		}
	}
	return one(l.skill.keyEdit(k, to))
}

// quoteLine quotes the value on a top-level key: value line of invalid YAML.
func (l *linter) quoteLine(idx int, key string) func([]string) ([]string, error) {
	end := l.fmEnd
	return func(lines []string) ([]string, error) {
		_, value, _ := strings.Cut(lines[idx], ":")
		lines[idx] = key + ": " + scalar(strings.TrimSpace(value))
		var doc yaml.Node
		if err := yaml.Unmarshal([]byte(strings.Join(lines[1:end], "\n")), &doc); err != nil {
			return nil, fmt.Errorf("the frontmatter is still invalid: %v", err)
		}
		return lines, nil
	}
}

// knownField finds the field a near-miss key (wrong case, _ for -) was meant to be.
func knownField(key string) string {
	norm := func(s string) string { return strings.ReplaceAll(strings.ToLower(s), "_", "-") }
	for _, f := range slices.Concat(specFields, claudeFields) {
		if norm(f) == norm(key) {
			return f
		}
	}
	return ""
}

// duplicateKeys lists the paths of keys defined twice in any mapping. yaml.v3 accepts
// them into a Node, but Pi's parser and Codex's serde reject the whole document.
func duplicateKeys(n *yaml.Node, path string) []string {
	var dups []string
	seen := map[string]bool{}
	for k, v := range pairs(n) {
		p := join(path, k.Value)
		if seen[k.Value] {
			dups = append(dups, p)
		}
		seen[k.Value] = true
		dups = append(dups, duplicateKeys(v, p)...)
	}
	if n.Kind == yaml.SequenceNode {
		for i, item := range n.Content {
			dups = append(dups, duplicateKeys(item, fmt.Sprintf("%s[%d]", path, i))...)
		}
	}
	return dups
}

func join(path, key string) string { return strings.TrimPrefix(path+"."+key, ".") }

// quotedNeedlessly reports a quoted string that YAML would read identically unquoted.
// Marshalling the value shows whether the encoder itself would need quotes for it; it
// keeps quotes on YAML 1.1 lookalikes (yes, on, 0755…), so the advice is safe for
// Codex's serde_yaml and Pi's parser too. Only valid in block context: inside flow
// collections, characters like "," and "]" need quotes the encoder doesn't know about.
func quotedNeedlessly(n *yaml.Node) bool {
	if n.Kind != yaml.ScalarNode || (n.Style != yaml.DoubleQuotedStyle && n.Style != yaml.SingleQuotedStyle) {
		return false
	}
	out, err := yaml.Marshal(n.Value)
	if err != nil {
		return false
	}
	return !strings.ContainsAny(string(out[:1]), `"'|>`)
}

func isString(n *yaml.Node) bool { return n.Kind == yaml.ScalarNode && n.Tag == "!!str" }
func isNull(n *yaml.Node) bool   { return n.Kind == yaml.ScalarNode && n.Tag == "!!null" }
func isBool(n *yaml.Node) bool {
	return n.Kind == yaml.ScalarNode && n.Style == 0 && (n.Value == "true" || n.Value == "false")
}

// utf16Len is the length Pi checks against the limits (JavaScript string length).
func utf16Len(s string) int { return len(utf16.Encode([]rune(s))) }

func (l *linter) checkName(n *yaml.Node) {
	dir := l.in.DirName
	var issue *Issue
	switch {
	case n == nil:
		issue = l.add(Error, "Frontmatter has no name (the spec requires one; harnesses fall back to the directory name)")
	case !isString(n):
		issue = l.add(Error, "name must be a string")
	case !namePattern.MatchString(n.Value) || len(n.Value) > maxNameChars:
		issue = l.add(Error, "Name %q must be lowercase letters, digits and single hyphens, at most %d characters", n.Value, maxNameChars)
	case n.Value != dir:
		issue = l.add(Error, "Name %q does not match the directory name %q", n.Value, dir)
	default:
		return
	}
	if !namePattern.MatchString(dir) || len(dir) > maxNameChars {
		return
	}
	if n == nil {
		issue.withFix(skillFile, "Add name", insert(1, "name: "+dir), "name")
	} else {
		issue.withFix(skillFile, "Use "+dir, one(l.skill.scalarEdit(n, dir)), "name")
	}
}

func (l *linter) checkDescription(n, whenToUse *yaml.Node) {
	if n == nil {
		l.add(Error, "Frontmatter has no description. Pi and Codex skip the skill, and nothing can trigger it")
		return
	}
	if !isString(n) {
		l.add(Error, "description must be a string")
		return
	}

	desc := n.Value
	l.res.Description = desc
	l.res.DescriptionChars = len([]rune(desc))
	switch length := max(l.res.DescriptionChars, utf16Len(desc)); {
	case strings.TrimSpace(desc) == "":
		l.add(Error, "Description is empty")
	case length > maxDescriptionChars:
		l.add(Error, "Description is %d characters as Pi counts them (the spec limit is %d)", length, maxDescriptionChars)
	case placeholderPattern.MatchString(desc):
		l.add(Warning, "Description still contains a placeholder (%s); agents match skills by this text", placeholderPattern.FindString(desc))
	case !l.res.ManualOnly && !triggerPattern.MatchString(desc):
		// A hint, not a rule: some descriptions make the trigger obvious without the phrasing.
		l.add(Info, `Consider a "Use when…" or "Read before…" sentence: the description says what the skill does but not when to use it`)
	}

	// `description: foo # bar` is valid YAML that silently drops everything from " #".
	if n.Style == 0 && n.LineComment != "" {
		issue := l.add(Warning, "The description ends in a YAML comment (%s), so harnesses only see %q. Quote the value if the # belongs to the text", n.LineComment, desc)
		idx := l.skill.at(n)
		line := l.skill.lines[idx]
		start := offset(line, n.Column)
		if raw := strings.TrimRight(line[start:], " \t"); strings.HasPrefix(raw, n.Value+" ") {
			issue.withFix(skillFile, "Quote value", one(edit{idx, start, start + len(raw), scalar(raw)}, true), "description")
		}
	}
	if strings.ContainsAny(desc, "<>") {
		l.add(Warning, "Claude Code shows < and > in descriptions as &lt; and &gt;; use [square brackets] for placeholders")
	}
	if whenToUse != nil && isString(whenToUse) {
		if total := l.res.DescriptionChars + len([]rune(whenToUse.Value)); total > maxClaudeListingChars {
			l.add(Warning, "description + when_to_use is %d characters; Claude Code truncates it at %d", total, maxClaudeListingChars)
		}
	}
}

func (l *linter) checkWhenToUse(desc, whenToUse *yaml.Node) {
	if whenToUse == nil {
		return
	}
	issue := l.add(Warning, "when_to_use is read only by Claude Code; Codex and Pi never see this trigger text. Fold it into description")
	if desc != nil && isString(desc) && isString(whenToUse) {
		joined := strings.TrimSpace(desc.Value) + " " + strings.TrimSpace(whenToUse.Value)
		issue.withFix(skillFile, "Fold into description",
			l.rewriteFields(map[string][]string{"description": field("description", joined), "when_to_use": nil}),
			"description", "when_to_use")
	}
}

func (l *linter) checkBooleans(fields map[string]*yaml.Node) {
	for _, key := range boolFields {
		n := fields[key]
		if n == nil {
			continue
		}
		if !isBool(n) {
			issue := l.add(Error, "%s must be exactly true or false. Pi honors only a literal true, while Claude Code also accepts yes, on or 1, so harnesses would disagree", key)
			if literal, ok := yamlBools[strings.ToLower(n.Value)]; ok {
				issue.withFix(skillFile, "Set to "+literal, one(l.skill.scalarEdit(n, literal)), key)
			}
			if key == "disable-model-invocation" {
				l.manualOnlyValid = false
			}
		} else if key == "disable-model-invocation" {
			l.res.ManualOnly = n.Value == "true"
		}
	}
}

func (l *linter) checkOptionalFields(fields map[string]*yaml.Node) {
	if n := fields["compatibility"]; n != nil {
		if !isString(n) || n.Value == "" || len([]rune(n.Value)) > maxCompatibilityChars {
			l.add(Error, "compatibility must be a string of 1–%d characters", maxCompatibilityChars)
		}
	}
	if n := fields["license"]; n != nil && !isString(n) {
		l.add(Warning, "license should be a string")
	}
	if n := fields["metadata"]; n != nil {
		// Codex decodes metadata into a struct (for metadata.short-description), so anything
		// but a mapping, or a non-string short-description, fails its whole frontmatter.
		if n.Kind != yaml.MappingNode {
			l.add(Error, "metadata must be a map of keys to string values; anything else makes Codex skip the skill")
		} else {
			for k, v := range pairs(n) {
				var issue *Issue
				switch {
				case isString(v):
					continue
				case k.Value == "short-description":
					issue = l.add(Error, "metadata.short-description must be a string, or Codex skips the skill")
				default:
					issue = l.add(Warning, "metadata.%s should be a string (the spec maps keys to string values; quote numbers like \"1.0\")", k.Value)
				}
				if v.Kind == yaml.ScalarNode && !isNull(v) {
					issue.withFix(skillFile, "Quote value", one(l.skill.scalarEdit(v, scalar(v.Value))), "metadata")
				}
			}
		}
	}
	if n := fields["allowed-tools"]; n != nil && n.Kind == yaml.SequenceNode {
		issue := l.add(Info, "allowed-tools is a list; the spec defines a space-separated string, and only Claude Code accepts both")
		tools := make([]string, 0, len(n.Content))
		for _, t := range n.Content {
			if isString(t) && t.Value != "" && !strings.ContainsAny(t.Value, " \t\n") {
				tools = append(tools, t.Value)
			}
		}
		if len(tools) > 0 && len(tools) == len(n.Content) { // a tool with spaces can't join into the string
			issue.withFix(skillFile, "Join into a string",
				l.rewriteFields(map[string][]string{"allowed-tools": field("allowed-tools", strings.Join(tools, " "))}),
				"allowed-tools")
		}
	}
}

// checkClaudeFields validates Claude Code extension values that have a fixed set of options.
func (l *linter) checkClaudeFields(fields map[string]*yaml.Node) {
	for key, allowed := range claudeEnums {
		if n := fields[key]; n != nil && !slices.Contains(allowed, n.Value) {
			l.add(Warning, "%s must be one of %s for Claude Code", key, strings.Join(allowed, ", "))
		}
	}
	if ctx := fields["context"]; ctx == nil || ctx.Value != "fork" {
		for _, key := range forkOnlyFields {
			if fields[key] != nil {
				l.add(Warning, "%s only takes effect with context: fork", key)
			}
		}
	}
}

// checkCodexPolicy compares disable-model-invocation (Claude Code, Pi) with Codex's
// own switch in agents/openai.yaml, so a skill is manual-only everywhere or nowhere.
func (l *linter) checkCodexPolicy(manualOnly *yaml.Node) {
	implicit := true // Codex's default, and what it falls back to when it ignores the file
	var allow *yaml.Node
	readable := true // Codex reads agents/openai.yaml (or it's absent), so editing it takes effect
	if l.in.OpenAIYAML != nil {
		if allow, readable = l.checkOpenAI(); allow != nil {
			implicit = allow.Value == "true"
		}
	}
	if !l.manualOnlyValid {
		return
	}
	switch {
	case l.res.ManualOnly && implicit:
		issue := l.add(Error, "Manual-only in Claude Code and Pi, but Codex still invokes it automatically. Set policy.allow_implicit_invocation: false in %s", openAIFile)
		label := "Update openai.yaml"
		if l.in.OpenAIYAML == nil {
			label = "Create openai.yaml"
		}
		if readable {
			issue.withFix(openAIFile, label, l.disableImplicit(allow), "policy")
		}
	case !l.res.ManualOnly && !implicit:
		l.add(Error, "Codex won't invoke it automatically (%s), but Claude Code and Pi will. Add disable-model-invocation: true, or drop the Codex policy", openAIFile).
			withFix(skillFile, "Make manual-only", l.enableManualOnly(manualOnly), "disable-model-invocation")
	}
}

// checkOpenAI lints agents/openai.yaml and returns the policy.allow_implicit_invocation
// node Codex will honor, or nil. Codex ignores the whole file when it fails to decode
// (wrong types, duplicate keys); ok is false then.
func (l *linter) checkOpenAI() (policy *yaml.Node, ok bool) {
	var doc yaml.Node
	if err := yaml.Unmarshal(l.in.OpenAIYAML, &doc); err != nil {
		l.add(Error, "%s is not valid YAML (%v), so Codex ignores it", openAIFile, err)
		return nil, false
	}
	if len(doc.Content) == 0 {
		return nil, true
	}
	root := doc.Content[0]
	checkFinalNewline(l, openAIFile, string(l.in.OpenAIYAML))
	l.checkStyle(l.openAI, string(l.in.OpenAIYAML), root)

	var broken []string // problems that make Codex ignore the whole file
	wrongType := func(p, want string) { broken = append(broken, fmt.Sprintf("%s must be %s", p, want)) }
	if dups := duplicateKeys(root, ""); len(dups) > 0 {
		broken = append(broken, strings.Join(dups, ", ")+" defined more than once")
	}

	if root.Kind != yaml.MappingNode {
		wrongType("the file", "a mapping")
	}
	for section, body := range pairs(root) {
		if section.Value == "policy" {
			l.policyKey, l.policyBody = section, body
		}
		if isNull(body) {
			continue
		}
		switch section.Value {
		case "interface":
			if body.Kind != yaml.MappingNode {
				wrongType("interface", "a mapping")
				continue
			}
			for k, v := range pairs(body) {
				p := join("interface", k.Value)
				limit, known := openAIInterface[k.Value]
				switch {
				case !known:
					l.add(Warning, "%s: unknown key %s", openAIFile, p)
				case isNull(v):
				case !isString(v):
					wrongType(p, "a string")
				case limit > 0 && len([]rune(v.Value)) > limit:
					l.add(Warning, "%s: %s is over %d characters, so Codex drops it", openAIFile, p, limit)
				case k.Value == "brand_color" && !brandColorPattern.MatchString(v.Value):
					l.add(Warning, "%s: %s must be #RRGGBB, so Codex drops it", openAIFile, p)
				}
			}
		case "policy":
			if body.Kind != yaml.MappingNode {
				wrongType("policy", "a mapping")
				continue
			}
			for k, v := range pairs(body) {
				p := join("policy", k.Value)
				switch k.Value {
				case "allow_implicit_invocation":
					if !isBool(v) {
						wrongType(p, "exactly true or false")
						continue
					}
					policy = v
					if v.Value == "true" {
						l.add(Warning, "%s: %s: true is Codex's default; remove it", openAIFile, p).
							withFix(openAIFile, "Remove", l.dropImplicitDefault(v), "policy")
					}
				case "products":
					if v.Kind != yaml.SequenceNode || slices.ContainsFunc(v.Content, func(n *yaml.Node) bool { return !isString(n) }) {
						wrongType(p, "a list of product names")
					}
				default:
					l.add(Warning, "%s: unknown key %s", openAIFile, p)
				}
			}
		case "dependencies":
			l.checkOpenAIDependencies(body, wrongType)
		default:
			l.add(Warning, "%s: unknown section %s; Codex reads only interface, policy and dependencies", openAIFile, section.Value)
		}
	}

	if len(broken) > 0 {
		l.add(Error, "%s: %s, so Codex ignores the whole file", openAIFile, strings.Join(broken, "; "))
		return nil, false
	}
	return policy, true
}

func (l *linter) checkOpenAIDependencies(body *yaml.Node, wrongType func(p, want string)) {
	if body.Kind != yaml.MappingNode {
		wrongType("dependencies", "a mapping")
		return
	}
	for k, tools := range pairs(body) {
		if k.Value != "tools" {
			l.add(Warning, "%s: unknown key dependencies.%s", openAIFile, k.Value)
			continue
		}
		if isNull(tools) {
			continue
		}
		if tools.Kind != yaml.SequenceNode {
			wrongType("dependencies.tools", "a list")
			continue
		}
		for i, tool := range tools.Content {
			p := fmt.Sprintf("dependencies.tools[%d]", i)
			if tool.Kind != yaml.MappingNode {
				wrongType(p, "a mapping")
				continue
			}
			seen := map[string]bool{}
			for tk, tv := range pairs(tool) {
				tp := join(p, tk.Value)
				seen[tk.Value] = true
				limit, known := openAIToolKeys[tk.Value]
				switch {
				case tk.Value == "oauth":
					if tv.Kind != yaml.MappingNode && !isNull(tv) {
						wrongType(tp, "a mapping")
					}
				case !known:
					l.add(Warning, "%s: unknown key %s", openAIFile, tp)
				case isNull(tv):
				case !isString(tv):
					wrongType(tp, "a string")
				case len([]rune(tv.Value)) > limit:
					l.add(Warning, "%s: %s is over %d characters, so Codex drops the dependency", openAIFile, tp, limit)
				}
			}
			for _, required := range []string{"type", "value"} {
				if !seen[required] {
					l.add(Warning, "%s: %s has no %s, so Codex drops the dependency", openAIFile, p, required)
				}
			}
		}
	}
}

// pairs iterates the key/value nodes of a mapping (nothing for any other node).
func pairs(n *yaml.Node) func(yield func(k, v *yaml.Node) bool) {
	return func(yield func(k, v *yaml.Node) bool) {
		if n.Kind != yaml.MappingNode {
			return
		}
		for i := 0; i+1 < len(n.Content); i += 2 {
			if !yield(n.Content[i], n.Content[i+1]) {
				return
			}
		}
	}
}

// checkOrder enforces the conventional field order: name, then description.
func (l *linter) checkOrder(root *yaml.Node) {
	var keys []string
	for k := range pairs(root) {
		keys = append(keys, k.Value)
	}
	if len(keys) >= 2 && (keys[0] != "name" || keys[1] != "description") {
		issue := l.add(Warning, "Put name first and description second; the other fields follow")
		if slices.Contains(keys, "name") && slices.Contains(keys, "description") { // otherwise reordering can't resolve it
			issue.withFix(skillFile, "Reorder", l.reorderFields())
		}
	}
}

// checkStyle enforces conventional, consistent YAML: no tab indentation or trailing
// spaces, 2-space indentation, and quotes only where YAML needs them. Reported line
// numbers are the file's, which is where s says the YAML text starts.
func (l *linter) checkStyle(s source, text string, root *yaml.Node) {
	var tabs, trailing []string
	lines := strings.Split(text, "\n")
	content := blockScalarLines(root, len(lines))
	for i, line := range lines {
		if content[i] {
			continue
		}
		indent := line[:len(line)-len(strings.TrimLeft(line, " \t"))]
		if strings.Contains(indent, "\t") {
			tabs = append(tabs, fmt.Sprint(s.first+1+i))
		}
		if strings.TrimRight(line, " \t") != line {
			trailing = append(trailing, fmt.Sprint(s.first+1+i))
		}
	}
	// The line-wise fixes edit only the YAML's own lines.
	eachLine := func(fn func(string) string) func([]string) ([]string, error) {
		return func(ls []string) ([]string, error) {
			for i := range lines {
				if !content[i] {
					ls[s.first+i] = fn(ls[s.first+i])
				}
			}
			return ls, nil
		}
	}
	if len(tabs) > 0 {
		l.add(Warning, "%s: tab indentation on line %s; indent with spaces", s.file, strings.Join(tabs, ", ")).
			withFix(s.file, "Use spaces", eachLine(func(line string) string {
				body := strings.TrimLeft(line, " \t")
				indent := line[:len(line)-len(body)]
				return strings.ReplaceAll(indent, "\t", strings.Repeat(" ", yamlIndent)) + body
			}))
	}
	if len(trailing) > 0 {
		l.add(Warning, "%s: trailing whitespace on line %s", s.file, strings.Join(trailing, ", ")).
			withFix(s.file, "Trim", eachLine(func(line string) string { return strings.TrimRight(line, " \t") }))
	}

	var quoted, misindented []string
	var unquote []edit
	unquotable := true
	var indentKeys, indentValues []*yaml.Node
	addQuoted := func(n *yaml.Node, p string) {
		quoted = append(quoted, p)
		e, ok := s.scalarEdit(n, scalar(n.Value))
		unquote = append(unquote, e)
		unquotable = unquotable && ok
	}
	var walk func(n *yaml.Node, path string, inFlow bool)
	walk = func(n *yaml.Node, path string, inFlow bool) {
		inFlow = inFlow || n.Style&yaml.FlowStyle != 0
		for k, v := range pairs(n) {
			p := join(path, k.Value)
			if !inFlow && quotedNeedlessly(k) {
				addQuoted(k, p)
			}
			if !inFlow && quotedNeedlessly(v) {
				addQuoted(v, p)
			}
			if !inFlow && v.Kind == yaml.MappingNode && v.Style&yaml.FlowStyle == 0 && len(v.Content) > 0 &&
				v.Content[0].Column != k.Column+yamlIndent {
				misindented = append(misindented, p)
				indentKeys, indentValues = append(indentKeys, k), append(indentValues, v)
			}
			walk(v, p, inFlow)
		}
		if n.Kind == yaml.SequenceNode {
			for i, item := range n.Content {
				p := fmt.Sprintf("%s[%d]", path, i)
				if !inFlow && quotedNeedlessly(item) {
					addQuoted(item, p)
				}
				walk(item, p, inFlow)
			}
		}
	}
	walk(root, "", false)

	if len(quoted) > 0 {
		// A quoted key and its quoted value share a path; name it once.
		issue := l.add(Warning, "%s: unnecessary quotes around %s; YAML reads these the same without them", s.file, strings.Join(slices.Compact(quoted), ", "))
		if unquotable {
			issue.withFix(s.file, "Remove quotes", applyEdits(unquote))
		}
	}
	if len(misindented) > 0 {
		l.add(Warning, "%s: indent the keys under %s by %d spaces", s.file, strings.Join(misindented, ", "), yamlIndent).
			withFix(s.file, "Reindent", s.reindent(indentKeys, indentValues))
	}
}

// blockScalarLines marks the content lines of literal (|) and folded (>) scalars, where
// tabs and trailing spaces are part of the value. A block scalar's content runs until
// the next node starts.
func blockScalarLines(root *yaml.Node, count int) []bool {
	var starts []int
	var blocks []*yaml.Node
	var walk func(n *yaml.Node)
	walk = func(n *yaml.Node) {
		starts = append(starts, n.Line)
		if n.Style&(yaml.LiteralStyle|yaml.FoldedStyle) != 0 {
			blocks = append(blocks, n)
		}
		for _, c := range n.Content {
			walk(c)
		}
	}
	walk(root)
	slices.Sort(starts)
	content := make([]bool, count)
	for _, b := range blocks {
		end := count + 1 // node lines are 1-based
		if i, found := slices.BinarySearch(starts, b.Line+1); found || i < len(starts) {
			end = starts[i]
		}
		for line := b.Line + 1; line < end && line <= count; line++ {
			content[line-1] = true
		}
	}
	return content
}

// checkFinalNewline flags a non-empty file whose last line has no newline.
func checkFinalNewline(l *linter, file, text string) {
	if text != "" && !strings.HasSuffix(text, "\n") {
		l.add(Warning, "%s: the file doesn't end with a newline", file).
			withFix(file, "Add newline", func(lines []string) ([]string, error) { return append(lines, ""), nil })
	}
}

func (l *linter) checkClaudeVariables(body string) {
	found := slices.Compact(slices.Sorted(slices.Values(claudeVarPattern.FindAllString(body, -1))))
	if len(found) > 0 {
		l.add(Info, "%s is expanded only by Claude Code; other harnesses pass it to the model literally", strings.Join(found, ", "))
	}
}

// checkScripts compares each script's executable bit with its #! line. A script without
// either is fine: agents run it through its interpreter, or it is a module others import.
func (l *linter) checkScripts() {
	for _, s := range l.in.Scripts {
		switch {
		case s.Shebang && !s.Executable:
			l.add(Warning, "%s starts with #! but is not executable, so running it directly fails. Run chmod +x on it", s.Path).
				withChmod(s.Path)
		case s.Executable && !s.Shebang:
			l.add(Info, "%s is executable but has no #! line, so running it directly hands it to /bin/sh", s.Path)
		}
	}
}

func (l *linter) checkLinks(body string) {
	exists := func(target string) bool {
		target = path.Clean(target)
		if target == ".." || strings.HasPrefix(target, "../") {
			return l.in.Outside(target)
		}
		for _, f := range l.in.Files {
			if f == target || strings.HasPrefix(f, target+"/") {
				return true
			}
		}
		return false
	}

	prose := codePattern.ReplaceAllString(body, "")
	links := slices.Concat(linkPattern.FindAllStringSubmatch(prose, -1), refLinkPattern.FindAllStringSubmatch(prose, -1))
	for _, m := range links {
		raw := strings.TrimSuffix(strings.TrimPrefix(m[1], "<"), ">")
		target, _, _ := strings.Cut(raw, "#")
		if target == "" || strings.Contains(target, "://") || strings.HasPrefix(target, "mailto:") {
			continue
		}
		if decoded, err := url.PathUnescape(target); err == nil {
			target = decoded
		}
		if !exists(target) {
			l.add(Error, "Broken link to %s", raw)
		}
	}

	for _, f := range l.in.Files {
		for _, dir := range linkedDirs {
			if strings.HasPrefix(f, dir) && !strings.Contains(body, f) {
				l.add(Info, "%s is never mentioned in SKILL.md, so agents may not know it exists", f)
			}
		}
	}
}
