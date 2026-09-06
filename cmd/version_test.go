package cmd

import (
	"bytes"
	"strings"
	"testing"
)

func TestFormatVersion(t *testing.T) {
	const rev = "186f3580a4e4c0ffee1234567890abcdef123456"
	for _, c := range []struct {
		name string
		main string
		rev  string
		dirt bool
		want string
		why  string
	}{{
		name: "released build names its tag",
		main: "v1.4.0", rev: rev,
		want: "v1.4.0 (186f3580a4e4)",
		why:  "a real tag says something the revision does not, so it survives",
	}, {
		name: "pseudo-version collapses to devel",
		main: "v0.0.0-20260906024537-186f3580a4e4", rev: rev,
		want: "devel (186f3580a4e4)",
		why:  "the pseudo-version only restates the revision printed beside it",
	}, {
		name: "dirty pseudo-version collapses and still says dirty",
		main: "v0.0.0-20260906024537-186f3580a4e4+dirty", rev: rev, dirt: true,
		want: "devel (186f3580a4e4, dirty)",
		why:  "a dirty build is not the commit it names and must say so once",
	}, {
		name: "dirty at a tag says dirty once, not twice",
		main: "v0.1.0+dirty", rev: rev, dirt: true,
		want: "v0.1.0 (186f3580a4e4, dirty)",
		why:  "+dirty is build metadata repeating what the dirty flag already reports",
	}, {
		name: "checkout with no module version",
		main: "(devel)", rev: rev,
		want: "devel (186f3580a4e4)",
	}, {
		name: "dirty with no revision still says dirty",
		main: "(devel)", dirt: true,
		want: "devel (dirty)",
		why:  "a stamped modification must never pass for a clean build",
	}, {
		name: "no vcs stamp at all",
		main: "v2.0.1",
		want: "v2.0.1",
	}} {
		t.Run(c.name, func(t *testing.T) {
			got := formatVersion("", c.main, c.rev, "", "", c.dirt)
			if got != c.want {
				t.Errorf("formatVersion(%q, rev=%t, dirty=%t) = %q, want %q\n%s",
					c.main, c.rev != "", c.dirt, got, c.want, c.why)
			}
		})
	}
}

// TestFormatVersion_AlwaysNamesTheBuildAndTheToolchain keeps the two trailing
// facts from being dropped as noise. They are what a bug report needs and
// cannot be recovered from a binary afterwards -- and for this program the
// toolchain matters twice over, since a render's behaviour can turn on which
// OpenCV and ffmpeg the build was linked and run against.
func TestFormatVersion_AlwaysNamesTheBuildAndTheToolchain(t *testing.T) {
	got := formatVersion("", "(devel)", "abc123def456789", "2026-09-06T02:45:37Z", "go1.26.5", false)
	for _, want := range []string{"2026-09-06T02:45:37Z", "go1.26.5", "abc123def456"} {
		if !strings.Contains(got, want) {
			t.Errorf("version %q does not report %q", got, want)
		}
	}
}

// TestVersionFlag_NeedsNoInputVideo pins cobra's ordering rather than this
// package's own code, deliberately.
//
// The root command declares MinimumNArgs(1) because applying an effect needs
// something to apply it to, so --version works only because cobra answers it
// before validating arguments. A cobra release that reordered those two would
// turn `videofx --version` into a complaint about a missing input file --
// exactly the breakage nobody thinks to check after a dependency bump.
func TestVersionFlag_NeedsNoInputVideo(t *testing.T) {
	root := NewRootCmd()
	if root.Version == "" {
		t.Fatal("root.Version is empty, so cobra adds no --version flag at all")
	}
	var out bytes.Buffer
	root.SetOut(&out)
	root.SetErr(&out)
	root.SetArgs([]string{"--version"})

	if err := root.Execute(); err != nil {
		t.Fatalf("videofx --version: %v", err)
	}
	if got := out.String(); !strings.Contains(got, "videofx version ") {
		t.Errorf("--version printed %q, want it to name the program and its version", got)
	}
}

// TestFormatVersion_DescribeWins covers the case the build wrappers create:
// a local build past a release, where `git describe` knows something the
// build information does not -- which release, and how far past it.
func TestFormatVersion_DescribeWins(t *testing.T) {
	const rev = "186f3580a4e4c0ffee1234567890abcdef123456"
	for _, c := range []struct {
		name string
		desc string
		dirt bool
		want string
		why  string
	}{{
		name: "past a release names the release",
		desc: "v0.1.0-1-g186f358",
		want: "v0.1.0-1-g186f358 built T with go1",
		why:  "this is the whole point: devel plus a hash does not say what it is near",
	}, {
		name: "the commit is not printed twice",
		desc: "v0.1.0-1-g186f358",
		want: "v0.1.0-1-g186f358 built T with go1",
		why:  "describe already ends in the commit, so the revision is suppressed",
	}, {
		name: "dirty is still reported, and only once",
		desc: "v0.1.0-1-g186f358", dirt: true,
		want: "v0.1.0-1-g186f358 (dirty) built T with go1",
		why:  "describe is deliberately not asked for --dirty, so this is the one spelling",
	}, {
		name: "exactly at a tag",
		desc: "v0.1.0",
		want: "v0.1.0 built T with go1",
	}} {
		t.Run(c.name, func(t *testing.T) {
			got := formatVersion(c.desc, "v0.1.1-0.20260906031330-186f358e7369", rev, "T", "go1", c.dirt)
			if got != c.want {
				t.Errorf("formatVersion(described=%q, dirty=%t) = %q, want %q\n%s",
					c.desc, c.dirt, got, c.want, c.why)
			}
			if strings.Count(got, "186f358") > 1 {
				t.Errorf("the commit appears more than once in %q", got)
			}
		})
	}
}
