package git

import (
	"os"
	"path/filepath"
	"testing"
)

// IsTracked is a SAFETY GATE input (the Seam A JSONL deletion guard refuses to
// delete a git-tracked source-of-truth mirror). Its old bool-only signature
// returned `err == nil`, so every way git can fail to answer — absent binary,
// safe.directory refusal, corrupt index, unreadable .git — became a confident
// "not tracked" and the gate FAILED OPEN.
//
// These tests attack that classification directly: each case asserts not just
// the bool but whether the answer is claimed to be DEFINITIVE (nil error) or
// UNKNOWN (non-nil error). A regression that restores `return err == nil` turns
// the two unknown cases below red.

// writeTrackedFixture returns a repo containing relPath, staged or not.
func writeTrackedFixture(t *testing.T, stage bool) (repo, relPath string) {
	t.Helper()
	repo = initTestRepo(t)
	relPath = ".beads/issues.jsonl"
	if err := os.MkdirAll(filepath.Join(repo, ".beads"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(repo, relPath), []byte("{}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if stage {
		runGit(t, repo, "add", relPath)
	}
	return repo, relPath
}

func TestIsTrackedReportsDefinitiveAnswers(t *testing.T) {
	t.Run("staged file is tracked", func(t *testing.T) {
		repo, relPath := writeTrackedFixture(t, true)
		tracked, err := New(repo).IsTracked(relPath)
		if err != nil || !tracked {
			t.Fatalf("IsTracked = (%v, %v), want (true, nil)", tracked, err)
		}
	})

	t.Run("unstaged file in a repo is definitively untracked", func(t *testing.T) {
		repo, relPath := writeTrackedFixture(t, false)
		tracked, err := New(repo).IsTracked(relPath)
		if err != nil || tracked {
			t.Fatalf("IsTracked = (%v, %v), want (false, nil) — ls-files exit 1 is git ANSWERING, not failing", tracked, err)
		}
	})

	t.Run("no repository above the dir is a definitive negative", func(t *testing.T) {
		// Nothing under a repo can be tracked, so this must NOT degrade to
		// unknown: doing so would jam the deletion gate permanently shut for
		// every non-repo scope.
		dir := t.TempDir()
		tracked, err := New(dir).IsTracked(".beads/issues.jsonl")
		if err != nil || tracked {
			t.Fatalf("IsTracked = (%v, %v), want (false, nil) for a non-repo directory", tracked, err)
		}
	})
}

// The heart of the finding: git failing to answer INSIDE a repository must
// surface as unknown, never as a confident "untracked".
func TestIsTrackedReportsUnknownWhenGitCannotAnswer(t *testing.T) {
	t.Run("corrupt index", func(t *testing.T) {
		repo, relPath := writeTrackedFixture(t, true)
		// The file IS tracked; only git's ability to say so is destroyed.
		if err := os.WriteFile(filepath.Join(repo, ".git", "index"), []byte("garbage"), 0o644); err != nil {
			t.Fatal(err)
		}
		tracked, err := New(repo).IsTracked(relPath)
		if err == nil {
			t.Fatalf("IsTracked = (%v, nil) with a corrupt index; want a non-nil error — a genuinely tracked file was reported untracked with no signal", tracked)
		}
		if tracked {
			t.Fatal("IsTracked returned true alongside an error; the bool is only meaningful when err is nil")
		}
	})

	t.Run("git binary absent while a repository exists", func(t *testing.T) {
		repo, relPath := writeTrackedFixture(t, true)
		t.Setenv("PATH", t.TempDir()) // no git anywhere on PATH
		tracked, err := New(repo).IsTracked(relPath)
		if err == nil {
			t.Fatalf("IsTracked = (%v, nil) with no git on PATH; want a non-nil error", tracked)
		}
	})

	t.Run("git binary absent and no repository is still definitive", func(t *testing.T) {
		// Both signals point the same way, so the negative stands: this keeps a
		// missing git from wedging every non-repo scope.
		dir := t.TempDir()
		t.Setenv("PATH", t.TempDir())
		tracked, err := New(dir).IsTracked(".beads/issues.jsonl")
		if err != nil || tracked {
			t.Fatalf("IsTracked = (%v, %v), want (false, nil)", tracked, err)
		}
	})

	t.Run("broken .git pointer file", func(t *testing.T) {
		// A linked worktree whose gitdir has gone away: `.git` exists, so a
		// repository cannot be ruled out on the filesystem either.
		dir := t.TempDir()
		if err := os.WriteFile(filepath.Join(dir, ".git"), []byte("gitdir: /nonexistent/gone\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		if _, err := New(dir).IsTracked(".beads/issues.jsonl"); err == nil {
			t.Fatal("IsTracked returned nil error for a repository git refused to open; want unknown")
		}
	})
}

func TestHasGitDirAncestor(t *testing.T) {
	t.Run("finds a .git directory on an ancestor", func(t *testing.T) {
		repo := initTestRepo(t)
		nested := filepath.Join(repo, "a", "b")
		if err := os.MkdirAll(nested, 0o755); err != nil {
			t.Fatal(err)
		}
		if inRepo, determinate := hasGitDirAncestor(nested); !inRepo || !determinate {
			t.Fatalf("hasGitDirAncestor = (%v, %v), want (true, true)", inRepo, determinate)
		}
	})

	t.Run("counts a .git FILE (linked worktree / submodule)", func(t *testing.T) {
		dir := t.TempDir()
		if err := os.WriteFile(filepath.Join(dir, ".git"), []byte("gitdir: elsewhere\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		if inRepo, determinate := hasGitDirAncestor(dir); !inRepo || !determinate {
			t.Fatalf("hasGitDirAncestor = (%v, %v), want (true, true) — a linked worktree's .git is a file", inRepo, determinate)
		}
	})

	t.Run("walks to the filesystem root and reports a determinate no", func(t *testing.T) {
		if inRepo, determinate := hasGitDirAncestor(t.TempDir()); inRepo || !determinate {
			t.Fatalf("hasGitDirAncestor = (%v, %v), want (false, true)", inRepo, determinate)
		}
	})
}

// A symlinked scope root must not turn an UNKNOWN git failure into a confident
// "untracked".
//
// git runs with cmd.Dir set to the caller's path, so the kernel follows the
// link and git operates inside the real repository. A LEXICAL ancestor walk
// climbs the symlink's own parents instead, finds no .git, and would report a
// determinate "no repository" — handing back (false, nil) for a file that is
// genuinely tracked, which is precisely the fail-open behavior this API exists
// to remove. Found by an independent review pass.
//
// The link MUST point at a SUBDIRECTORY of the repo, not its root: the walk's
// first Lstat is on "<link>/.git", which already traverses the link, so a link
// to the root would find .git even without symlink resolution and the test
// would pass for the wrong reason. With .git one level ABOVE the link target,
// only a resolved walk can reach it.
func TestIsTrackedFollowsSymlinkedWorkDirWhenGitCannotAnswer(t *testing.T) {
	repo := initTestRepo(t)
	scope := filepath.Join(repo, "sub")
	relPath := ".beads/issues.jsonl"
	if err := os.MkdirAll(filepath.Join(scope, ".beads"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(scope, relPath), []byte("{}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	runGit(t, repo, "add", filepath.Join("sub", relPath))

	link := filepath.Join(t.TempDir(), "scopelink")
	if err := os.Symlink(scope, link); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	// Guard the guard: .git must NOT be reachable at <link>/.git, or the
	// lexical walk would succeed by accident and prove nothing.
	if _, err := os.Lstat(filepath.Join(link, ".git")); err == nil {
		t.Fatal("fixture broken: <link>/.git exists, so this test cannot detect an unresolved ancestor walk")
	}

	// Sanity: with git healthy, the link resolves and the file reads tracked.
	if tracked, err := New(link).IsTracked(relPath); err != nil || !tracked {
		t.Fatalf("IsTracked through symlink = (%v, %v), want (true, nil)", tracked, err)
	}

	// Now destroy git's ability to answer. The file is STILL tracked.
	if err := os.WriteFile(filepath.Join(repo, ".git", "index"), []byte("garbage"), 0o644); err != nil {
		t.Fatal(err)
	}
	tracked, err := New(link).IsTracked(relPath)
	if err == nil {
		t.Fatalf("IsTracked = (%v, nil) for a symlinked scope root whose repo git refused to read; want unknown — an unresolved ancestor walk sees no .git above the link and would authorize deleting a tracked file", tracked)
	}
}

func TestHasGitDirAncestorResolvesSymlinks(t *testing.T) {
	repo := initTestRepo(t)
	scope := filepath.Join(repo, "sub")
	if err := os.MkdirAll(scope, 0o755); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(t.TempDir(), "scopelink")
	if err := os.Symlink(scope, link); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	if _, err := os.Lstat(filepath.Join(link, ".git")); err == nil {
		t.Fatal("fixture broken: .git must live ABOVE the link target")
	}
	if inRepo, determinate := hasGitDirAncestor(link); !inRepo || !determinate {
		t.Fatalf("hasGitDirAncestor(symlink into a repo subdir) = (%v, %v), want (true, true)", inRepo, determinate)
	}
}

func TestHasGitDirAncestorIsIndeterminateForAnUnresolvablePath(t *testing.T) {
	// A dangling symlink: we cannot establish where it points, so a repository
	// cannot be ruled out. Fail closed rather than claim a definitive negative.
	dir := t.TempDir()
	link := filepath.Join(dir, "dangling")
	if err := os.Symlink(filepath.Join(dir, "gone"), link); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	if inRepo, determinate := hasGitDirAncestor(link); inRepo || determinate {
		t.Fatalf("hasGitDirAncestor(dangling symlink) = (%v, %v), want (false, false)", inRepo, determinate)
	}
}
