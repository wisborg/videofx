package cmd

import (
	"runtime/debug"
	"strings"
)

// described is set at link time by the Makefile to `git describe --tags`,
// e.g. "v0.1.0-1-ge9c0288" -- the last release, how far past it this commit
// is, and which commit.
//
// It exists because the build information alone cannot answer "which release
// is this near". The toolchain records the revision but not the nearest tag,
// so a build one commit past a release reports only "devel", and finding out
// what that is near takes a second lookup in the repository. That is a poor
// answer to somebody holding a binary and asking what it is.
//
// This is NOT the hardcoded version string this file otherwise refuses. It is
// derived from the repository at build time, by the same git the VCS stamp
// comes from, so it cannot be forgotten or bumped wrongly -- the failure mode
// of a written-down version is that somebody has to remember it, and nobody
// has to remember this.
//
// Empty when built any other way (a bare `go build`, which this project
// warns against for its own reasons, or `go install` from a published tag).
// Both fall back to the build information, which for an installed release
// names the tag outright and needs no help.
var described string

// version reports what this binary actually is, read from the build
// information the Go toolchain embeds rather than from a constant in the
// source.
//
// A hardcoded version string is a value that has to be remembered, and the
// failure it produces is silent: a binary confidently naming the release
// before the one it was actually cut from, with nothing to reveal the
// mistake. Reading it from the build means the fact is never written down
// twice, so it cannot come to disagree with itself.
//
// This matters more here than in most programs. A render takes a long time
// and its output is a file somebody keeps, sometimes long after the source
// has moved on; being able to ask a finished render's producer what it was
// is how a result gets traced back to the code that made it.

func version() string {
	info, ok := debug.ReadBuildInfo()
	if !ok {
		// Only reachable in a binary built without module support at all.
		// Saying so is better than inventing a number.
		return "unknown (no build information embedded)"
	}
	var revision, built string
	var dirty bool
	for _, s := range info.Settings {
		switch s.Key {
		case "vcs.revision":
			revision = s.Value
		case "vcs.time":
			built = s.Value
		case "vcs.modified":
			dirty = s.Value == "true"
		}
	}
	return formatVersion(described, info.Main.Version, revision, built, info.GoVersion, dirty)
}

// formatVersion composes the reported version from the four facts the build
// carries. It is separate from version() so the judgements below can be
// tested against inputs a test can actually produce -- a test binary's own
// build information describes the test binary, not a release, so it can
// never exercise the case that matters most.
//
// The judgements, in order:
//
//   - A module version is worth printing only when it names a RELEASE. Built
//     from a checkout, the toolchain synthesises a pseudo-version that merely
//     restates the revision and the dirty flag, so printing it beside them
//     says everything twice and buries the one word a reader wants, which is
//     that this is not a release at all. It is detected by asking whether it
//     contains the revision -- which is exactly the question being asked,
//     does this version add anything the revision has not already said,
//     rather than a guess at the toolchain's format.
//
//   - vcs.modified is reported prominently, because a binary built from a
//     dirty tree IS NOT the commit it names, and a bug report quoting that
//     commit would send somebody to read source that was never compiled.
//     It is reported even when no revision came with it, so a stamped
//     modification can never pass for a clean build.
func formatVersion(described, mainVersion, revision, built, goVersion string, dirty bool) string {
	// A git describe string already names the commit ("...-ge9c0288"), so
	// the revision is suppressed rather than printed again beside it. What
	// stays is the dirty flag, which describe is deliberately not asked for
	// (--dirty) precisely so there is one spelling of that fact and not two.
	if described != "" {
		return joinVersion(described, "", built, goVersion, dirty)
	}

	// The toolchain appends "+dirty" as semver build metadata when the tree
	// was modified. Dirtiness is already reported beside the revision, in one
	// place, for every build -- so keeping the suffix as well prints the same
	// fact twice and makes a tagged dirty build read as though "+dirty" were
	// part of the release's name, which it is not. The suffix goes; the
	// dirty flag is what reports it.
	v := strings.TrimSuffix(mainVersion, "+dirty")
	if v == "" || v == "(devel)" || (revision != "" && strings.Contains(v, shortRevision(revision))) {
		v = "devel"
	}
	return joinVersion(v, revision, built, goVersion, dirty)
}

// joinVersion assembles the reported line from an already-chosen version
// string. An empty revision means the version already names the commit.
func joinVersion(v, revision, built, goVersion string, dirty bool) string {
	parts := []string{v}

	switch {
	case revision != "":
		short := shortRevision(revision)
		if dirty {
			short += ", dirty"
		}
		parts = append(parts, "("+short+")")
	case dirty:
		parts = append(parts, "(dirty)")
	}

	if built != "" {
		parts = append(parts, "built "+built)
	}
	if goVersion != "" {
		parts = append(parts, "with "+goVersion)
	}
	return strings.Join(parts, " ")
}

// shortRevision abbreviates a commit hash to twelve characters -- the length
// git itself grows an abbreviated hash to on a repository of any size, and
// unambiguous well past anything this project will reach.
func shortRevision(rev string) string {
	if len(rev) > 12 {
		return rev[:12]
	}
	return rev
}
