package lint

import (
	"cmp"
	"errors"
	"fmt"
	"maps"
	"reflect"
	"slices"
	"strings"
	"unicode/utf8"

	"go.yaml.in/yaml/v3"
)

// ErrNotFixed means an automatic fix was not applied: the issue has none, the files
// changed since it was reported, or the edit would not have resolved it cleanly.
var ErrNotFixed = errors.New("cannot fix automatically")

// Fix is an automatic edit that resolves an issue: either a text edit of one file, or
// making a script executable. Text edits change source lines in place rather than
// re-encoding the YAML, so comments and layout survive.
type Fix struct {
	Label  string `json:"label"`
	file   string // skillFile or openAIFile
	apply  func(lines []string) ([]string, error)
	script string // instead of a text edit: the script to make executable
	// The top-level keys whose values the fix means to change. Every other value must
	// decode the same after the edit; for most fixes (style) that is all of them.
	changes []string
}

func (i *Issue) withFix(file, label string, apply func([]string) ([]string, error), changes ...string) {
	if apply != nil {
		i.Fix = &Fix{Label: label, file: file, apply: apply, changes: changes}
	}
}

func (i *Issue) withChmod(script string) {
	i.Fix = &Fix{Label: "Make executable", script: script}
}

// ApplyFix lints in, applies the fix of the issue with this message and returns the
// edited input. The edited file comes back the way the linter reads it: LF line endings,
// no BOM. An openai.yaml left empty comes back nil; a script made executable comes back
// with Executable set. It refuses an edit that leaves the issue in place or causes a new error.
func ApplyFix(in Input, message string) (Input, error) {
	before := lintSkill(in)
	i := slices.IndexFunc(before.res.Issues, func(i Issue) bool { return i.Message == message && i.Fix != nil })
	if i < 0 {
		return in, fmt.Errorf("%w: no fix for %q; the files may have changed", ErrNotFixed, message)
	}
	fix := before.res.Issues[i].Fix
	out, err := fix.edit(in, before)
	if err != nil {
		return in, fmt.Errorf("%w: %v", ErrNotFixed, err)
	}
	if err := fix.keepsData(in, out); err != nil {
		return in, fmt.Errorf("%w: %v", ErrNotFixed, err)
	}

	after := lintSkill(out)
	for _, issue := range after.res.Issues {
		if issue.Message == message {
			return in, fmt.Errorf("%w: the edit did not resolve %q", ErrNotFixed, message)
		}
		// Unparsed frontmatter skips most checks, so fixing it is expected to surface new errors.
		isNew := !slices.ContainsFunc(before.res.Issues, func(b Issue) bool { return b.Message == issue.Message })
		if before.parsed && issue.Severity == Error && isNew {
			return in, fmt.Errorf("%w: the edit would cause a new error: %s", ErrNotFixed, issue.Message)
		}
	}
	return out, nil
}

// keepsData checks that the edit changed no YAML value other than the ones the fix names.
// Lint messages alone can't show that a text edit damaged an unrelated value.
func (fix *Fix) keepsData(in, out Input) error {
	if fix.script != "" {
		return nil
	}
	old, err := decode(in, fix.file)
	if err != nil {
		return nil // nothing to compare against: fixing invalid YAML
	}
	updated, err := decode(out, fix.file)
	if err != nil {
		return fmt.Errorf("the edit would make %s invalid YAML: %v", fix.file, err)
	}
	for _, key := range slices.Concat(slices.Collect(maps.Keys(old)), slices.Collect(maps.Keys(updated))) {
		if !slices.Contains(fix.changes, key) && !reflect.DeepEqual(old[key], updated[key]) {
			return fmt.Errorf("the edit would change %s in %s", key, fix.file)
		}
	}
	return nil
}

// decode reads the top-level YAML values of SKILL.md's frontmatter or of openai.yaml.
func decode(in Input, file string) (map[string]any, error) {
	text := string(in.OpenAIYAML)
	if file == skillFile {
		content := strings.TrimPrefix(strings.ReplaceAll(in.Content, "\r\n", "\n"), bom)
		yamlText, _, _, ok := splitFrontmatter(content)
		if !ok {
			return nil, errors.New("no frontmatter")
		}
		text = yamlText
	}
	var data map[string]any
	err := yaml.Unmarshal([]byte(text), &data)
	return data, err
}

func (fix *Fix) edit(in Input, before *linter) (Input, error) {
	out := in
	if fix.script != "" {
		out.Scripts = slices.Clone(in.Scripts)
		for i := range out.Scripts {
			if out.Scripts[i].Path == fix.script {
				out.Scripts[i].Executable = true
			}
		}
		return out, nil
	}

	src, original := before.skill, in.Content
	if fix.file == openAIFile {
		src, original = before.openAI, string(in.OpenAIYAML)
	}
	lines, err := fix.apply(slices.Clone(src.lines))
	if err != nil {
		return in, err
	}
	// The lines were split from LF-normalised text; write back the file's own line endings.
	eol := "\n"
	if strings.Contains(original, "\r\n") {
		eol = "\r\n"
	}
	if text := strings.Join(lines, eol); fix.file == skillFile {
		out.Content = text
	} else if strings.TrimSpace(text) != "" {
		out.OpenAIYAML = []byte(text)
	} else {
		out.OpenAIYAML = nil // nothing left: the file goes
	}
	return out, nil
}

// source is a linted file split into lines, the unit fixes edit.
type source struct {
	file  string
	lines []string
	first int // index of the line the YAML starts on
}

// at is the index of a node's line.
func (s source) at(n *yaml.Node) int { return s.first + n.Line - 1 }

// edit replaces lines[line][start:end] with text.
type edit struct {
	line, start, end int
	text             string
}

func applyEdits(edits []edit) func([]string) ([]string, error) {
	return func(lines []string) ([]string, error) {
		// Back to front, so the offsets of the remaining edits stay valid.
		slices.SortFunc(edits, func(a, b edit) int { return cmp.Or(b.line-a.line, b.start-a.start) })
		for _, e := range edits {
			lines[e.line] = lines[e.line][:e.start] + e.text + lines[e.line][e.end:]
		}
		return lines, nil
	}
}

// one turns a single edit into a fix, or nil when the edit isn't possible.
func one(e edit, ok bool) func([]string) ([]string, error) {
	if !ok {
		return nil
	}
	return applyEdits([]edit{e})
}

func insert(at int, line string) func([]string) ([]string, error) {
	return func(lines []string) ([]string, error) { return slices.Insert(lines, at, line), nil }
}

// scalarEdit replaces a scalar's source text. It only handles scalars on a single line:
// block scalars and values that continue on the next line aren't safe to edit as text.
func (s source) scalarEdit(n *yaml.Node, text string) (edit, bool) {
	if n.Kind != yaml.ScalarNode || n.Line < 1 {
		return edit{}, false
	}
	idx := s.at(n)
	line := s.lines[idx]
	start := offset(line, n.Column)
	if start >= len(line) {
		return edit{}, false
	}
	end := -1
	switch n.Style {
	case 0:
		raw := line[start:]
		if n.LineComment != "" {
			raw, _, _ = strings.Cut(raw, " #")
		}
		if raw = strings.TrimRight(raw, " \t"); raw == n.Value {
			end = start + len(raw)
		}
	case yaml.DoubleQuotedStyle, yaml.SingleQuotedStyle:
		end = closingQuote(line, start)
	}
	if end < 0 {
		return edit{}, false
	}
	return edit{idx, start, end, text}, true
}

// offset converts a yaml.v3 column (1-based, counted in characters) to a byte offset.
func offset(line string, column int) int {
	i := 0
	for n := 1; n < column && i < len(line); n++ {
		_, size := utf8.DecodeRuneInString(line[i:])
		i += size
	}
	return i
}

// keyEdit replaces a plain mapping key.
func (s source) keyEdit(k *yaml.Node, text string) (edit, bool) {
	idx := s.at(k)
	start := offset(s.lines[idx], k.Column)
	if k.Style != 0 || !strings.HasPrefix(s.lines[idx][start:], k.Value+":") {
		return edit{}, false
	}
	return edit{idx, start, start + len(k.Value), text}, true
}

// closingQuote returns the offset just past the quoted scalar that opens at start, or -1
// when it doesn't close on the same line.
func closingQuote(line string, start int) int {
	q := line[start]
	if q != '"' && q != '\'' {
		return -1
	}
	for i := start + 1; i < len(line); i++ {
		switch {
		case q == '"' && line[i] == '\\':
			i++
		case line[i] != q:
		case q == '\'' && i+1 < len(line) && line[i+1] == '\'': // '' is an escaped quote
			i++
		default:
			return i + 1
		}
	}
	return -1
}

// scalar is how the YAML encoder writes a single-line string: quoted only when needed.
func scalar(v string) string {
	out, _ := yaml.Marshal(v) // a plain string always encodes
	return strings.TrimSuffix(string(out), "\n")
}

// field writes a top-level key: value, in block style when the value has line breaks.
func field(key, value string) []string {
	var b strings.Builder
	enc := yaml.NewEncoder(&b)
	enc.SetIndent(yamlIndent)
	_ = enc.Encode(map[string]string{key: value}) // a string map always encodes
	_ = enc.Close()
	return strings.Split(strings.TrimSuffix(b.String(), "\n"), "\n")
}

// span is the lines [start, end) one top-level frontmatter field takes up.
type span struct {
	key        string
	start, end int
}

// fieldSpans locates each top-level frontmatter field. Blank lines and comments at the
// end of a field belong to the field after it.
func (l *linter) fieldSpans() []span {
	var spans []span
	for k := range pairs(l.root) {
		spans = append(spans, span{key: k.Value, start: l.skill.at(k)})
	}
	for i := range spans {
		end := l.fmEnd
		if i+1 < len(spans) {
			end = spans[i+1].start
		}
		for end-1 > spans[i].start && (strings.TrimSpace(l.skill.lines[end-1]) == "" || strings.HasPrefix(l.skill.lines[end-1], "#")) {
			end--
		}
		spans[i].end = end
	}
	return spans
}

// rewriteFields replaces the lines of top-level frontmatter fields; nil removes one.
// The new lines are built from values, so it declines fields that carry comments.
func (l *linter) rewriteFields(repl map[string][]string) func([]string) ([]string, error) {
	if l.dupKeys {
		return nil
	}
	for k, v := range pairs(l.root) {
		if _, ok := repl[k.Value]; ok && (hasComments(k) || hasComments(v)) {
			return nil
		}
	}
	spans := l.fieldSpans()
	return func(lines []string) ([]string, error) {
		for _, sp := range slices.Backward(spans) {
			if r, ok := repl[sp.key]; ok {
				lines = slices.Replace(lines, sp.start, sp.end, r...)
			}
		}
		return lines, nil
	}
}

func hasComments(n *yaml.Node) bool {
	return n.HeadComment != "" || n.LineComment != "" || n.FootComment != "" || slices.ContainsFunc(n.Content, hasComments)
}

// reorderFields moves name and description to the top. Each field keeps the comments
// above it.
func (l *linter) reorderFields() func([]string) ([]string, error) {
	if l.dupKeys {
		return nil
	}
	spans := l.fieldSpans()
	leading := []string{"name", "description"}
	return func(lines []string) ([]string, error) {
		segment := func(i int) []string {
			from := spans[i].start
			if i > 0 {
				from = spans[i-1].end
			}
			return lines[from:spans[i].end]
		}
		out := slices.Clone(lines[:spans[0].start])
		for _, key := range leading {
			if i := slices.IndexFunc(spans, func(sp span) bool { return sp.key == key }); i >= 0 {
				out = append(out, segment(i)...)
			}
		}
		for i, sp := range spans {
			if !slices.Contains(leading, sp.key) {
				out = append(out, segment(i)...)
			}
		}
		return append(out, lines[spans[len(spans)-1].end:]...), nil
	}
}

// reindent shifts the lines under each key so its nested mapping is indented by
// yamlIndent. Nested shifts add up: an inner mapping moves with its parent.
func (s source) reindent(keys, values []*yaml.Node) func([]string) ([]string, error) {
	return func(lines []string) ([]string, error) {
		shift := make([]int, len(lines))
		for i, k := range keys {
			delta := k.Column + yamlIndent - values[i].Content[0].Column
			for j := s.at(k) + 1; j < len(lines); j++ {
				if strings.TrimSpace(lines[j]) == "" {
					continue
				}
				if leadingSpaces(lines[j]) <= k.Column-1 {
					break
				}
				shift[j] += delta
			}
		}
		for j, d := range shift {
			n := leadingSpaces(lines[j])
			if n+d < 0 {
				return nil, fmt.Errorf("line %d is indented too little to shift", j+1)
			}
			lines[j] = strings.Repeat(" ", n+d) + lines[j][n:]
		}
		return lines, nil
	}
}

func leadingSpaces(line string) int { return len(line) - len(strings.TrimLeft(line, " ")) }

// disableImplicit sets policy.allow_implicit_invocation: false in agents/openai.yaml,
// adding the policy section, or the whole file, when it's missing.
func (l *linter) disableImplicit(allow *yaml.Node) func([]string) ([]string, error) {
	const setting = "allow_implicit_invocation: false"
	s, key, body := l.openAI, l.policyKey, l.policyBody
	switch {
	case allow != nil:
		return one(s.scalarEdit(allow, "false"))
	case key == nil:
		return func(lines []string) ([]string, error) {
			for len(lines) > 0 && lines[len(lines)-1] == "" {
				lines = lines[:len(lines)-1]
			}
			return append(lines, "policy:", strings.Repeat(" ", yamlIndent)+setting, ""), nil
		}
	case body.Kind == yaml.MappingNode && body.Style&yaml.FlowStyle == 0 && len(body.Content) > 0:
		return insert(s.at(key)+1, strings.Repeat(" ", body.Content[0].Column-1)+setting)
	case isNull(body) && strings.TrimSpace(s.lines[s.at(key)]) == "policy:":
		return insert(s.at(key)+1, strings.Repeat(" ", key.Column-1+yamlIndent)+setting)
	}
	return nil
}

// dropImplicitDefault removes allow_implicit_invocation: true, and the policy section
// with it when nothing else is in there.
func (l *linter) dropImplicitDefault(allow *yaml.Node) func([]string) ([]string, error) {
	s := l.openAI
	if l.policyBody.Style&yaml.FlowStyle != 0 { // {allow_implicit_invocation: true, …}
		return nil
	}
	from := s.at(allow)
	if len(l.policyBody.Content) == 2 {
		from = s.at(l.policyKey)
	}
	return func(lines []string) ([]string, error) { return slices.Delete(lines, from, s.at(allow)+1), nil }
}

// enableManualOnly sets disable-model-invocation: true in the frontmatter.
func (l *linter) enableManualOnly(n *yaml.Node) func([]string) ([]string, error) {
	if n != nil {
		return one(l.skill.scalarEdit(n, "true"))
	}
	return insert(l.fmEnd, "disable-model-invocation: true")
}
