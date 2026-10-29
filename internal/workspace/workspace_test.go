package workspace

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"testing"
)

func mkfile(t *testing.T, p, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestFindProjects(t *testing.T) {
	home := t.TempDir()
	global := filepath.Join(home, ".agents")
	repo := filepath.Join(home, "dev", "mono")
	pkg := filepath.Join(repo, "frontend")
	mkfile(t, filepath.Join(home, "dev", "AGENTS.md"), "") // above the repo root: never loaded
	mkfile(t, filepath.Join(repo, ".git", "HEAD"), "")
	mkfile(t, filepath.Join(repo, "AGENTS.md"), "")
	mkfile(t, filepath.Join(pkg, ".agents", "skills", "x", "SKILL.md"), "")
	mkfile(t, filepath.Join(pkg, "src", "a.ts"), "")
	mkfile(t, filepath.Join(global, "AGENTS.md"), "")

	tests := []struct {
		name, from string
		want       []string
	}{
		{"monorepo package lists every level up to the repo root", filepath.Join(pkg, "src"), []string{pkg, repo}},
		{"repo root", repo, []string{repo}},
		{"no repo walks up to home but not into it", filepath.Join(home, "dev", "loose"), []string{filepath.Join(home, "dev")}},
		{"inside the global agents directory", filepath.Join(global, "skills"), nil},
		{"home itself", home, nil},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if err := os.MkdirAll(tt.from, 0o755); err != nil {
				t.Fatal(err)
			}
			if got := FindProjects(tt.from, home, global); !slices.Equal(got, tt.want) {
				t.Errorf("got %v, want %v", got, tt.want)
			}
		})
	}
}

func TestAbsRefusesUnmanagedPaths(t *testing.T) {
	w := Project("project-1", t.TempDir())
	for _, p := range []string{
		"project-1/AGENTS.md",
		"project-1/.agents/skills/x/SKILL.md",
		"project-1/.agents/skills-purgatory/x/SKILL.md",
	} {
		if _, err := w.Abs(p); err != nil {
			t.Errorf("%s: %v", p, err)
		}
	}
	for _, p := range []string{
		"project-1/src/a.ts",
		"project-1/.agents/skills/../../src/a.ts",
		"project-1/../other/AGENTS.md",
		"project-1/.agents/skillsX/a",
		"project-1/",
		"global/AGENTS.md",
	} {
		if _, err := w.Abs(p); !errors.Is(err, ErrOutsideRoot) {
			t.Errorf("%s: got %v, want ErrOutsideRoot", p, err)
		}
	}
}

func TestSearchFollowsSymlinkedSkillsDir(t *testing.T) {
	root := t.TempDir()
	mkfile(t, filepath.Join(root, ".claude", "skills", "x", "SKILL.md"), "find the needle\n")
	if err := os.MkdirAll(filepath.Join(root, ".agents"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("../.claude/skills", filepath.Join(root, ".agents", "skills")); err != nil {
		t.Fatal(err)
	}
	hits, err := Project("project-1", root).Search("needle")
	if err != nil {
		t.Fatal(err)
	}
	if len(hits) != 1 || hits[0].Path != "project-1/.agents/skills/x/SKILL.md" {
		t.Errorf("got %+v", hits)
	}
}

func TestMarkShadowed(t *testing.T) {
	trees := []Tree{
		{Workspace: Workspace{Kind: KindProject}, Skills: []Skill{{Name: "same", Enabled: true}, {Name: "own", Enabled: true}}},
		{Workspace: Workspace{Kind: KindGlobal}, Skills: []Skill{{Name: "same", Enabled: true}}},
	}
	MarkShadowed(trees)
	if len(trees[0].Skills[0].Issues) != 1 || len(trees[0].Skills[1].Issues) != 0 || len(trees[1].Skills[0].Issues) != 0 {
		t.Errorf("got %+v", trees)
	}
}

func TestDeleteSkill(t *testing.T) {
	root := t.TempDir()
	w := Project("project-1", root)
	mkfile(t, filepath.Join(root, ".agents", "skills", "x", "references", "a.md"), "")
	mkfile(t, filepath.Join(root, ".agents", "skills-purgatory", "y", "SKILL.md"), "")

	for _, dir := range []string{
		"project-1/.agents/skills",               // the skills directory itself
		"project-1/.agents/skills/x/references", // inside a skill
		"project-1/AGENTS.md",
		"project-1/.agents/skills/../../src",
	} {
		if err := w.DeleteSkill(dir); !errors.Is(err, ErrOutsideRoot) {
			t.Errorf("%s: got %v, want ErrOutsideRoot", dir, err)
		}
	}
	for _, dir := range []string{"project-1/.agents/skills/x", "project-1/.agents/skills-purgatory/y"} {
		if err := w.DeleteSkill(dir); err != nil {
			t.Fatalf("%s: %v", dir, err)
		}
		if exists(filepath.Join(root, filepath.FromSlash(dir[len("project-1/"):]))) {
			t.Errorf("%s still exists", dir)
		}
	}
	if err := w.DeleteSkill("project-1/.agents/skills/gone"); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("missing skill: got %v", err)
	}
}

func TestTreeMarksSkillsTheDotfilesIgnore(t *testing.T) {
	home := t.TempDir()
	gitDir := filepath.Join(t.TempDir(), "cfg")
	if out, err := exec.Command("git", "init", "--bare", gitDir).CombinedOutput(); err != nil {
		t.Fatalf("%v: %s", err, out)
	}
	mkfile(t, filepath.Join(home, ".gitignore"), ".agents/skills/secret/\n.agents/skills-purgatory/old*\n")
	for _, dir := range []string{"skills/shared", "skills/secret", "skills-purgatory/older"} {
		mkfile(t, filepath.Join(home, ".agents", dir, "SKILL.md"), "")
	}
	w := Global(filepath.Join(home, ".agents"))
	w.Dotfiles = []string{"--git-dir", gitDir, "--work-tree", home}

	tree, err := w.Tree()
	if err != nil {
		t.Fatal(err)
	}
	var local []string
	for _, s := range tree.Skills {
		if s.Local {
			local = append(local, s.Name)
		}
	}
	if want := []string{"secret", "older"}; !slices.Equal(local, want) {
		t.Errorf("local skills %v, want %v", local, want)
	}
}
