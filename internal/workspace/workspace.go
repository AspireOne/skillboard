// Package workspace reads and changes a set of agent instructions and skills: the
// global ~/.agents, or a project's own AGENTS.md and .agents/skills. Each has
// instruction files, enabled skills, and skills parked in purgatory.
package workspace

import (
	"bufio"
	"bytes"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"sync"
	"unicode/utf8"

	"skillboard/internal/lint"
)

const (
	SkillFile        = "SKILL.md"
	openAIFile       = "agents/openai.yaml"
	instructionsFile = "AGENTS.md"
	projectAgentsDir = ".agents"

	maxSearchResults = 200
	newSkillTemplate = "---\nname: %s\ndescription: TODO. Use when TODO.\n---\n\n# %s\n"
)

var (
	ErrConflict    = errors.New("file changed on disk since it was loaded")
	ErrExists      = errors.New("already exists")
	ErrNotText     = errors.New("not a UTF-8 text file")
	ErrInvalidName = errors.New("skill names are lowercase letters, digits and single hyphens")
	ErrOutsideRoot = errors.New("path is not an instruction file or inside a skills directory")

	skillNamePattern = regexp.MustCompile(`^[a-z0-9]+(-[a-z0-9]+)*$`)
	// Generated clutter that is never worth showing. Dot-entries are skipped separately.
	ignoredNames = []string{"__pycache__", "node_modules"}
)

type Kind string

const (
	KindGlobal  Kind = "global"
	KindProject Kind = "project"
)

type Workspace struct {
	// The first segment of every path the workspace hands out or accepts, so paths stay
	// unique across workspaces: "global/skills/x/SKILL.md".
	ID    string `json:"id"`
	Kind  Kind   `json:"kind"`
	Label string `json:"label"`
	Root  string `json:"root"`
	// Relative to Root.
	SkillsDir    string `json:"skillsDir"`
	PurgatoryDir string `json:"-"`
	// Instruction files listed above the skills; nil lists every file directly in Root.
	Instructions []string `json:"-"`
	// Git flags that reach the dotfiles repository, like --git-dir and --work-tree.
	// Skills it ignores are machine-local. Nil when there is no such repository.
	Dotfiles []string `json:"-"`
}

func Global(root string) Workspace {
	return Workspace{ID: "global", Kind: KindGlobal, Label: "Global", Root: root, SkillsDir: "skills", PurgatoryDir: "skills-purgatory"}
}

// Project is one directory's own instructions and skills, which harnesses load on top of
// the global ones. Its root is part of a repository, so only these paths are exposed.
func Project(id, root string) Workspace {
	return Workspace{
		ID:           id,
		Kind:         KindProject,
		Label:        filepath.Base(root),
		Root:         root,
		SkillsDir:    projectAgentsDir + "/skills",
		PurgatoryDir: projectAgentsDir + "/skills-purgatory",
		Instructions: []string{instructionsFile},
	}
}

// FindProjects lists, nearest first, every directory with an AGENTS.md or .agents from dir
// up to the repository root: harnesses load all of them, like a monorepo's root AGENTS.md
// next to a package's own. The walk stops before home and the global agents directory,
// whose AGENTS.md is the global one.
func FindProjects(dir, home, globalRoot string) []string {
	var dirs []string
	for dir != home && dir != globalRoot {
		if exists(filepath.Join(dir, instructionsFile)) || exists(filepath.Join(dir, projectAgentsDir)) {
			dirs = append(dirs, dir)
		}
		parent := filepath.Dir(dir)
		if exists(filepath.Join(dir, ".git")) || parent == dir {
			break
		}
		dir = parent
	}
	return dirs
}

func exists(p string) bool {
	_, err := os.Lstat(p)
	return err == nil
}

type File struct {
	Path  string `json:"path"`  // workspace ID, then the slash-separated path relative to Root
	Mtime int64  `json:"mtime"` // microseconds: nanoseconds exceed JavaScript's exact integer range
	Size  int64  `json:"size"`
}

type Skill struct {
	Name    string `json:"name"`
	Enabled bool   `json:"enabled"`
	Dir     string `json:"dir"` // a path like File.Path
	Files   []File `json:"files"`
	Local   bool   `json:"local"` // ignored by the dotfiles repository, so it stays on this machine
	lint.Result
}

type Tree struct {
	Workspace
	RootFiles []File  `json:"rootFiles"`
	Skills    []Skill `json:"skills"`
}

type SearchHit struct {
	Path string `json:"path"`
	Line int    `json:"line"`
	Text string `json:"text"`
}

func skipped(name string) bool {
	return strings.HasPrefix(name, ".") || slices.Contains(ignoredNames, name)
}

// Abs resolves a workspace path and refuses anything the workspace does not manage.
func (w Workspace) Abs(p string) (string, error) {
	rel, ok := strings.CutPrefix(p, w.ID+"/")
	rel = path.Clean(rel)
	if !ok || rel == "." || rel == ".." || strings.HasPrefix(rel, "../") || !w.manages(rel) {
		return "", fmt.Errorf("%q: %w", p, ErrOutsideRoot)
	}
	return filepath.Join(w.Root, filepath.FromSlash(rel)), nil
}

// manages reports whether a clean path relative to Root is an instruction file or lies
// inside one of the skills directories.
func (w Workspace) manages(rel string) bool {
	if w.Instructions == nil && !strings.Contains(rel, "/") || slices.Contains(w.Instructions, rel) {
		return true
	}
	return strings.HasPrefix(rel, w.SkillsDir+"/") || strings.HasPrefix(rel, w.PurgatoryDir+"/")
}

// path turns an absolute path inside Root into a workspace path.
func (w Workspace) path(abs string) string {
	rel, _ := filepath.Rel(w.Root, abs)
	return w.ID + "/" + filepath.ToSlash(rel)
}

func (w Workspace) fileOf(abs string, info fs.FileInfo) File {
	return File{Path: w.path(abs), Mtime: info.ModTime().UnixMicro(), Size: info.Size()}
}

func (w Workspace) Tree() (Tree, error) {
	tree := Tree{Workspace: w, RootFiles: []File{}, Skills: []Skill{}}

	names, err := w.instructionFiles()
	if err != nil {
		return tree, err
	}
	for _, name := range names {
		abs := filepath.Join(w.Root, name)
		info, err := os.Stat(abs)
		if err != nil {
			return tree, err
		}
		tree.RootFiles = append(tree.RootFiles, w.fileOf(abs, info))
	}

	for _, group := range []struct {
		dir     string
		enabled bool
	}{{w.SkillsDir, true}, {w.PurgatoryDir, false}} {
		entries, err := os.ReadDir(filepath.Join(w.Root, group.dir))
		if errors.Is(err, fs.ErrNotExist) {
			continue
		}
		if err != nil {
			return tree, err
		}
		for _, e := range entries {
			if !e.IsDir() || skipped(e.Name()) {
				continue
			}
			skill, err := w.skill(group.dir, e.Name(), group.enabled)
			if err != nil {
				return tree, err
			}
			tree.Skills = append(tree.Skills, skill)
		}
	}
	return tree, w.markLocal(tree.Skills)
}

// markLocal asks the dotfiles repository which skills it ignores.
func (w Workspace) markLocal(skills []Skill) error {
	if w.Dotfiles == nil || len(skills) == 0 {
		return nil
	}
	dirs := make([]string, len(skills))
	for i, s := range skills {
		dirs[i] = filepath.Join(w.Root, filepath.FromSlash(strings.TrimPrefix(s.Dir, w.ID+"/")))
	}
	cmd := exec.Command("git", append(slices.Clone(w.Dotfiles), "check-ignore", "--stdin")...)
	cmd.Stdin = strings.NewReader(strings.Join(dirs, "\n"))
	out, err := cmd.Output()
	// Exit status 1 means nothing is ignored.
	if exit := (*exec.ExitError)(nil); errors.As(err, &exit) && exit.ExitCode() == 1 {
		return nil
	}
	if err != nil {
		return fmt.Errorf("asking the dotfiles repository what it ignores: %w", err)
	}
	ignored := strings.Split(strings.TrimSpace(string(out)), "\n")
	for i := range skills {
		skills[i].Local = slices.Contains(ignored, dirs[i])
	}
	return nil
}

// instructionFiles lists the instruction files that exist, relative to Root.
func (w Workspace) instructionFiles() ([]string, error) {
	var names []string
	if w.Instructions != nil {
		for _, name := range w.Instructions {
			// Stat, not Lstat: a dangling symlink is skipped instead of failing the tree.
			if _, err := os.Stat(filepath.Join(w.Root, name)); err == nil {
				names = append(names, name)
			}
		}
		return names, nil
	}
	entries, err := os.ReadDir(w.Root)
	if err != nil {
		return nil, err
	}
	for _, e := range entries {
		if !e.IsDir() && !skipped(e.Name()) {
			names = append(names, e.Name())
		}
	}
	return names, nil
}

func (w Workspace) skill(groupDir, name string, enabled bool) (Skill, error) {
	absDir := filepath.Join(w.Root, filepath.FromSlash(groupDir), name)
	skill := Skill{Name: name, Enabled: enabled, Dir: w.path(absDir)}
	in, files, err := w.readSkill(absDir)
	skill.Files = files
	if err != nil {
		return skill, err
	}
	skill.Result = lint.Skill(in)
	return skill, nil
}

// readSkill lists a skill's files and reads everything the linter needs.
func (w Workspace) readSkill(absDir string) (lint.Input, []File, error) {
	files := []File{}
	var inSkill []string
	var scripts []lint.Script
	err := filepath.WalkDir(absDir, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if p != absDir && skipped(d.Name()) {
			if d.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		if d.IsDir() {
			return nil
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(absDir, p)
		inSkill = append(inSkill, filepath.ToSlash(rel))
		files = append(files, w.fileOf(p, info))
		if strings.HasPrefix(filepath.ToSlash(rel), "scripts/") {
			script, err := scriptOf(p, filepath.ToSlash(rel))
			if err != nil {
				return err
			}
			scripts = append(scripts, script)
		}
		return nil
	})
	if err != nil {
		return lint.Input{}, files, err
	}

	content, err := os.ReadFile(filepath.Join(absDir, SkillFile))
	hasSkillFile := err == nil
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return lint.Input{}, files, err
	}
	openAI, err := os.ReadFile(filepath.Join(absDir, filepath.FromSlash(openAIFile)))
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return lint.Input{}, files, err
	}
	return lint.Input{
		DirName:      filepath.Base(absDir),
		Content:      string(content),
		HasSkillFile: hasSkillFile,
		Files:        inSkill,
		OpenAIYAML:   openAI,
		Scripts:      scripts,
		Outside:      func(rel string) bool { return exists(filepath.Join(absDir, filepath.FromSlash(rel))) },
	}, files, nil
}

// fixing serializes fixes: each reads, edits and writes back whole files, so two at
// once would lose one of them.
var fixing sync.Mutex

// FixSkill applies the automatic fix of one of a skill's lint issues, named by its message.
func (w Workspace) FixSkill(dir, message string) error {
	fixing.Lock()
	defer fixing.Unlock()
	abs, err := w.skillDir(dir)
	if err != nil {
		return err
	}
	in, _, err := w.readSkill(abs)
	if err != nil {
		return err
	}
	out, err := lint.ApplyFix(in, message)
	if err != nil {
		return err
	}
	if out.Content != in.Content {
		if err := writeKeepingMode(filepath.Join(abs, SkillFile), []byte(out.Content)); err != nil {
			return err
		}
	}
	openAI := filepath.Join(abs, filepath.FromSlash(openAIFile))
	switch {
	case out.OpenAIYAML == nil && in.OpenAIYAML != nil:
		if err := os.Remove(openAI); err != nil {
			return err
		}
		_ = os.Remove(filepath.Dir(openAI)) // only succeeds when agents/ is now empty
	case !bytes.Equal(out.OpenAIYAML, in.OpenAIYAML):
		if err := writeKeepingMode(openAI, out.OpenAIYAML); err != nil {
			return err
		}
	}
	for i, script := range out.Scripts {
		if script.Executable && !in.Scripts[i].Executable {
			if err := makeExecutable(filepath.Join(abs, filepath.FromSlash(script.Path))); err != nil {
				return err
			}
		}
	}
	slog.Info("fixed lint issue", "skill", abs, "issue", message)
	return nil
}

// makeExecutable is chmod +x restricted to whoever can read the file: each read bit
// gains its execute bit, so no one gets access they didn't have.
func makeExecutable(abs string) error {
	info, err := os.Stat(abs) // through a symlink, like chmod
	if err != nil {
		return err
	}
	mode := info.Mode().Perm()
	return os.Chmod(abs, mode|(mode&0o444)>>2)
}

// writeKeepingMode writes a file in place (keeping its inode and permissions), creating
// it and its directory when missing.
func writeKeepingMode(abs string, data []byte) error {
	mode := fs.FileMode(0o644)
	if info, err := os.Stat(abs); err == nil {
		mode = info.Mode().Perm()
	}
	if err := os.MkdirAll(filepath.Dir(abs), 0o755); err != nil {
		return err
	}
	return os.WriteFile(abs, data, mode)
}

// scriptOf reads what the linter needs about a script: its executable bit (through any
// symlink) and whether it starts with #!.
func scriptOf(abs, rel string) (lint.Script, error) {
	info, err := os.Stat(abs)
	if err != nil {
		return lint.Script{}, err
	}
	f, err := os.Open(abs)
	if err != nil {
		return lint.Script{}, err
	}
	defer f.Close()
	head := make([]byte, 2)
	n, _ := io.ReadFull(f, head) // a shorter file simply has no #!
	return lint.Script{Path: rel, Executable: info.Mode()&0o111 != 0, Shebang: string(head[:n]) == "#!"}, nil
}

func (w Workspace) Read(p string) (string, File, error) {
	abs, err := w.Abs(p)
	if err != nil {
		return "", File{}, err
	}
	data, err := os.ReadFile(abs)
	if err != nil {
		return "", File{}, err
	}
	info, err := os.Stat(abs)
	if err != nil {
		return "", File{}, err
	}
	if !utf8.Valid(data) || slices.Contains(data, 0) {
		return "", w.fileOf(abs, info), ErrNotText
	}
	return string(data), w.fileOf(abs, info), nil
}

// Write saves content. baseMtime is the mtime the editor loaded; nil creates a new file.
// A mismatch means something else (often an agent) changed the file meanwhile.
func (w Workspace) Write(p, content string, baseMtime *int64) (File, error) {
	abs, err := w.Abs(p)
	if err != nil {
		return File{}, err
	}

	if baseMtime == nil {
		if err := os.MkdirAll(filepath.Dir(abs), 0o755); err != nil {
			return File{}, err
		}
		f, err := os.OpenFile(abs, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o644)
		if errors.Is(err, fs.ErrExist) {
			return File{}, fmt.Errorf("%s: %w", p, ErrExists)
		}
		if err != nil {
			return File{}, err
		}
		_, err = f.WriteString(content)
		if cerr := f.Close(); err == nil {
			err = cerr
		}
		if err != nil {
			return File{}, err
		}
		slog.Info("created file", "path", abs)
	} else {
		info, err := os.Stat(abs)
		if err != nil {
			return File{}, err
		}
		if info.ModTime().UnixMicro() != *baseMtime {
			return File{}, ErrConflict
		}
		// WriteFile keeps the inode, so symlinks pointing at this file stay intact.
		if err := os.WriteFile(abs, []byte(content), info.Mode().Perm()); err != nil {
			return File{}, err
		}
		slog.Info("saved file", "path", abs)
	}

	info, err := os.Stat(abs)
	if err != nil {
		return File{}, err
	}
	return w.fileOf(abs, info), nil
}

// SetEnabled moves a skill between the skills and purgatory directories and returns
// its new directory.
func (w Workspace) SetEnabled(name string, enabled bool) (string, error) {
	// Existing skills may break the naming convention (the linter reports that),
	// so only reject names that are not a single plain directory entry.
	if name != filepath.Base(name) || skipped(name) {
		return "", fmt.Errorf("%q: %w", name, ErrOutsideRoot)
	}
	from, to := w.PurgatoryDir, w.SkillsDir
	if !enabled {
		from, to = w.SkillsDir, w.PurgatoryDir
	}
	src := filepath.Join(w.Root, filepath.FromSlash(from), name)
	dst := filepath.Join(w.Root, filepath.FromSlash(to), name)
	if exists(dst) {
		return "", fmt.Errorf("%s/%s: %w", to, name, ErrExists)
	}
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return "", err
	}
	if err := os.Rename(src, dst); err != nil {
		return "", err
	}
	slog.Info("moved skill", "root", w.Root, "skill", name, "from", from, "to", to)
	return w.path(dst), nil
}

// DeleteSkill removes a skill directory, enabled or parked, with everything in it. A
// symlinked skill loses only its link.
func (w Workspace) DeleteSkill(dir string) error {
	abs, err := w.skillDir(dir)
	if err != nil {
		return err
	}
	if _, err := os.Lstat(abs); err != nil {
		return err
	}
	if err := os.RemoveAll(abs); err != nil {
		return err
	}
	slog.Info("deleted skill", "path", abs)
	return nil
}

// skillDir resolves the path of a skill directory, enabled or parked.
func (w Workspace) skillDir(dir string) (string, error) {
	abs, err := w.Abs(dir)
	if err != nil {
		return "", err
	}
	parent := filepath.Dir(abs)
	if parent != filepath.Join(w.Root, filepath.FromSlash(w.SkillsDir)) &&
		parent != filepath.Join(w.Root, filepath.FromSlash(w.PurgatoryDir)) {
		return "", fmt.Errorf("%q is not a skill directory: %w", dir, ErrOutsideRoot)
	}
	return abs, nil
}

func (w Workspace) CreateSkill(name string) (File, error) {
	if !skillNamePattern.MatchString(name) {
		return File{}, ErrInvalidName
	}
	for _, dir := range []string{w.SkillsDir, w.PurgatoryDir} {
		if exists(filepath.Join(w.Root, filepath.FromSlash(dir), name)) {
			return File{}, fmt.Errorf("%s/%s: %w", dir, name, ErrExists)
		}
	}
	return w.Write(w.ID+"/"+w.SkillsDir+"/"+name+"/"+SkillFile, fmt.Sprintf(newSkillTemplate, name, name), nil)
}

// Search does a case-insensitive substring search over the instruction files and skills.
func (w Workspace) Search(query string) ([]SearchHit, error) {
	hits := []SearchHit{}
	needle := strings.ToLower(query)
	if needle == "" {
		return hits, nil
	}
	starts, err := w.instructionFiles()
	if err != nil {
		return hits, err
	}
	starts = append(starts, w.SkillsDir, w.PurgatoryDir)
	for _, start := range starts {
		logical := filepath.Join(w.Root, filepath.FromSlash(start))
		// WalkDir does not follow a symlinked start, like .agents/skills -> ../.claude/skills.
		start, err := filepath.EvalSymlinks(logical)
		if errors.Is(err, fs.ErrNotExist) {
			continue // no purgatory yet
		}
		if err != nil {
			return hits, err
		}
		err = filepath.WalkDir(start, func(p string, d fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if p != start && skipped(d.Name()) {
				if d.IsDir() {
					return filepath.SkipDir
				}
				return nil
			}
			if d.IsDir() || len(hits) >= maxSearchResults {
				return nil
			}
			f, err := os.Open(p)
			if err != nil {
				return err
			}
			defer f.Close()
			scanner := bufio.NewScanner(f)
			for n := 1; scanner.Scan() && len(hits) < maxSearchResults; n++ {
				line := scanner.Text()
				if !utf8.ValidString(line) {
					return nil // binary file
				}
				if strings.Contains(strings.ToLower(line), needle) {
					rel, _ := filepath.Rel(start, p)
					hits = append(hits, SearchHit{w.path(filepath.Join(logical, rel)), n, strings.TrimSpace(line)})
				}
			}
			return nil // over-long lines end the scan early; good enough for search
		})
		if err != nil {
			return hits, err
		}
	}
	return hits, nil
}

// MarkShadowed warns on project skills that share a name with an enabled global skill:
// Claude Code gives the personal (global) skill precedence, so the project one never runs there.
func MarkShadowed(trees []Tree) {
	global := map[string]bool{}
	for _, t := range trees {
		if t.Kind != KindGlobal {
			continue
		}
		for _, s := range t.Skills {
			if s.Enabled {
				global[s.Name] = true
			}
		}
	}
	for _, t := range trees {
		if t.Kind != KindProject {
			continue
		}
		for i := range t.Skills {
			if s := &t.Skills[i]; global[s.Name] {
				s.Issues = append(s.Issues, lint.Issue{Severity: lint.Warning,
					Message: "A global skill has the same name; Claude Code runs the global one instead"})
			}
		}
	}
}
