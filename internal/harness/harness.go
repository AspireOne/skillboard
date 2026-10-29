// Package harness checks that each agent harness's own config paths point back at
// the shared agents directory, so every harness sees the same skills and instructions.
package harness

import (
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
)

type Status string

const (
	Linked  Status = "linked"  // resolves to the shared target
	Missing Status = "missing" // nothing at the path
	Drifted Status = "drifted" // something else lives there (a real copy, or a link elsewhere)
)

type link struct {
	harness string
	path    string // relative to Set.Base
	target  string // relative to Set.TargetRoot
}

// Paths where a harness does not read ~/.agents natively and needs a symlink into it.
// Codex reads ~/.agents/skills itself, so a ~/.codex/skills directory would only shadow it.
var globalLinks = []link{
	{"Claude Code", ".claude/skills", "skills"},
	{"Claude Code", ".claude/CLAUDE.md", "AGENTS.md"},
	{"Codex", ".codex/AGENTS.md", "AGENTS.md"},
	{"OpenCode", ".config/opencode/AGENTS.md", "AGENTS.md"},
	{"Pi", ".pi/agent/AGENTS.md", "AGENTS.md"},
}

// The same idea inside a project; every other harness reads .agents/skills natively.
var projectLinks = []link{
	{"Claude Code", ".claude/skills", ".agents/skills"},
}

// Set is the links of one workspace.
type Set struct {
	Scope      string // the workspace ID
	Base       string
	Shown      string // how Base is displayed, e.g. "~/"
	TargetRoot string
	SkillsDir  string // the target that holds the skills, relative to TargetRoot
	links      []link
}

func Global(home, agentsRoot string) Set {
	return Set{Scope: "global", Base: home, Shown: "~/", TargetRoot: agentsRoot, SkillsDir: "skills", links: globalLinks}
}

func Project(scope, root string) Set {
	return Set{Scope: scope, Base: root, Shown: filepath.Base(root) + "/", TargetRoot: root, SkillsDir: ".agents/skills", links: projectLinks}
}

type Check struct {
	Scope   string `json:"scope"`
	Harness string `json:"harness"`
	Base    string `json:"base"` // display prefix for Path
	Path    string `json:"path"`
	Target  string `json:"target"`
	Status  Status `json:"status"`
	Detail  string `json:"detail"`
}

// Checks covers only links whose target exists: a project without .agents/skills needs
// no .claude/skills link.
func (s Set) Checks() []Check {
	checks := []Check{}
	for _, l := range s.links {
		want, err := filepath.EvalSymlinks(filepath.Join(s.TargetRoot, l.target))
		if err != nil {
			continue
		}
		c := Check{Scope: s.Scope, Harness: l.harness, Base: s.Shown, Path: l.path, Target: l.target}
		at := filepath.Join(s.Base, l.path)
		if _, err := os.Lstat(at); err != nil {
			c.Status = Missing
		} else if got, err := filepath.EvalSymlinks(at); err != nil {
			c.Status, c.Detail = Drifted, "broken symlink"
		} else if got != want {
			c.Status, c.Detail = Drifted, "resolves to "+got
		} else {
			c.Status = Linked
		}
		checks = append(checks, c)
	}
	return checks
}

// Create makes the link at path. The link is relative, so it survives moving the
// directory that holds both ends. It never replaces anything already there.
func (s Set) Create(path string) error {
	i := slices.IndexFunc(s.links, func(l link) bool { return l.path == path })
	if i < 0 {
		return fmt.Errorf("no harness link at %q: %w", path, fs.ErrNotExist)
	}
	at := filepath.Join(s.Base, path)
	to := filepath.Join(s.TargetRoot, s.links[i].target)
	if _, err := os.Stat(to); err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(at), 0o755); err != nil {
		return err
	}
	rel, err := filepath.Rel(filepath.Dir(at), to)
	if err != nil {
		return err
	}
	if err := os.Symlink(rel, at); err != nil {
		return err
	}
	slog.Info("created harness link", "path", at, "target", rel)
	return nil
}

// LinkSkills creates the missing links to the skills directory, which can appear when a
// skill is created or restored from purgatory.
func (s Set) LinkSkills() error {
	for _, c := range s.Checks() {
		if c.Status != Missing || c.Target != s.SkillsDir {
			continue
		}
		if err := s.Create(c.Path); err != nil {
			return err
		}
	}
	return nil
}
