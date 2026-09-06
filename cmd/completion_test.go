package cmd

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestCompletionShells(t *testing.T) {
	for _, c := range []struct {
		in      string
		shell   string // $SHELL during the call
		want    []string
		wantErr bool
	}{
		{in: "zsh", want: []string{"zsh"}},
		{in: "bash", want: []string{"bash"}},
		{in: "all", want: []string{"zsh", "bash"}},
		{in: "auto", shell: "/bin/zsh", want: []string{"zsh"}},
		{in: "auto", shell: "/usr/local/bin/bash", want: []string{"bash"}},
		// An unrecognised or absent $SHELL installs for both rather than
		// guessing. A file the user's shell ignores is harmless; picking the
		// wrong shell and reporting success is not.
		{in: "auto", shell: "/usr/bin/fish", want: []string{"zsh", "bash"}},
		{in: "auto", shell: "", want: []string{"zsh", "bash"}},
		{in: "tcsh", wantErr: true},
	} {
		t.Run(c.in+"_"+c.shell, func(t *testing.T) {
			t.Setenv("SHELL", c.shell)
			got, err := completionShells(c.in)
			if c.wantErr {
				if err == nil {
					t.Fatalf("completionShells(%q) accepted an unknown shell", c.in)
				}
				return
			}
			if err != nil {
				t.Fatalf("completionShells(%q): %v", c.in, err)
			}
			if strings.Join(got, ",") != strings.Join(c.want, ",") {
				t.Errorf("completionShells(%q) with SHELL=%q = %v, want %v", c.in, c.shell, got, c.want)
			}
		})
	}
}

// TestScoreZshCompletionDir pins the two rules that were got wrong first
// time. Both produce a file that exists and does nothing, which is the
// failure this command was written to prevent.
func TestScoreZshCompletionDir(t *testing.T) {
	const home = "/home/someone"
	cache := home + "/.oh-my-zsh/cache/completions"
	homeComp := home + "/.oh-my-zsh/completions"
	sysSite := "/opt/homebrew/share/zsh/site-functions"

	if got := scoreZshCompletionDir(cache, home); got != 0 {
		t.Errorf("a cache directory scored %d, want 0 -- oh-my-zsh empties it, so a script there works until it silently does not", got)
	}
	if got := scoreZshCompletionDir(home+"/.zsh/cache", home); got != 0 {
		t.Errorf("a directory named cache scored %d, want 0", got)
	}
	if got := scoreZshCompletionDir(home+"/.oh-my-zsh/functions", home); got != 0 {
		t.Errorf("a plain function directory scored %d, want 0 -- completion scripts do not belong there", got)
	}
	if scoreZshCompletionDir(homeComp, home) <= scoreZshCompletionDir(sysSite, home) {
		t.Error("a system directory beat one under the user's home; home should win -- it needs no privileges and survives a package manager upgrading the shell")
	}
	if scoreZshCompletionDir(sysSite, home) <= 0 {
		t.Error("a conventional system site-functions directory was refused outright")
	}
}

func TestCompletionTargetFor_DirOverrideAndFilenames(t *testing.T) {
	dir := t.TempDir()

	z, err := completionTargetFor("zsh", dir, "videofx")
	if err != nil {
		t.Fatalf("zsh target: %v", err)
	}
	// zsh autoloads by function name, so the underscore is not decoration.
	if z.file != "_videofx" || z.dir != dir {
		t.Errorf("zsh target = %s/%s, want %s/_videofx", z.dir, z.file, dir)
	}

	b, err := completionTargetFor("bash", dir, "videofx")
	if err != nil {
		t.Fatalf("bash target: %v", err)
	}
	if b.file != "videofx" || b.dir != dir {
		t.Errorf("bash target = %s/%s, want %s/fitdash", b.dir, b.file, dir)
	}

	if _, err := completionTargetFor("fish", "", "videofx"); err == nil {
		t.Error("an unsupported shell was accepted")
	}
}

// TestRunCompletionInstall_WritesAScriptThatMentionsTheProgram is the one
// that would catch an empty or wrong-program file being written -- a check
// that the path exists would pass on a zero-byte file.
func TestRunCompletionInstall_WritesAScriptThatMentionsTheProgram(t *testing.T) {
	for _, shell := range []string{"zsh", "bash"} {
		t.Run(shell, func(t *testing.T) {
			dir := t.TempDir()
			rt := NewRootCmd()
			var out strings.Builder
			if err := runCompletionInstall(rt, shell, dir, false, &out); err != nil {
				t.Fatalf("install: %v", err)
			}
			name := rt.Name()
			if shell == "zsh" {
				name = "_" + name
			}
			b, err := os.ReadFile(filepath.Join(dir, name))
			if err != nil {
				t.Fatalf("reading the installed script: %v", err)
			}
			if len(b) == 0 {
				t.Fatal("the installed script is empty")
			}
			if !strings.Contains(string(b), rt.Name()) {
				t.Errorf("the installed script never mentions %q, so it completes something else", rt.Name())
			}
			if !strings.Contains(out.String(), dir) {
				t.Errorf("the report does not say where it wrote; got %q", out.String())
			}
		})
	}
}

// TestRunCompletionInstall_DryRunWritesNothing pins the promise --dry-run
// makes. A dry run that left a file behind would be worse than no flag.
func TestRunCompletionInstall_DryRunWritesNothing(t *testing.T) {
	dir := t.TempDir()
	var out strings.Builder
	if err := runCompletionInstall(NewRootCmd(), "zsh", dir, true, &out); err != nil {
		t.Fatalf("dry run: %v", err)
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Errorf("--dry-run wrote %d file(s) into %s", len(entries), dir)
	}
	if !strings.Contains(out.String(), "would write") {
		t.Errorf("--dry-run did not say what it would have done; got %q", out.String())
	}
}

// TestRunCompletionInstall_DirNeedsOneShell refuses an instruction that
// cannot be obeyed rather than picking one of the two shells and writing a
// script the other shell cannot use into the same file.
func TestRunCompletionInstall_DirNeedsOneShell(t *testing.T) {
	var out strings.Builder
	if err := runCompletionInstall(NewRootCmd(), "all", t.TempDir(), true, &out); err == nil {
		t.Error("--dir with --shell all was accepted; the two scripts would collide")
	}
}
