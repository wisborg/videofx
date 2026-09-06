package cmd

import (
	"runtime/debug"
	"strings"
)

// version reports what this binary actually is, read from the build
// information the Go toolchain embeds rather than from a constant in the
// source.
//
// A hardcoded version string is a value somebody has to remember to bump, and
// the failure it produces is silent: a binary confidently naming the release
// before the one it was actually cut from, with nothing anywhere to reveal
// the mistake. Reading it from the build means the fact is never written down
// twice, so it cannot come to disagree with itself.
//
// This matters more here than in most programs. videofx renders take a long
// time and their output is a file somebody keeps; being able to ask a
// finished render's producer what it was is how a result gets traced back to
// the code that made it.
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
	return formatVersion(info.Main.Version, revision, built, info.GoVersion, dirty)
}

// formatVersion composes the reported version from the four facts the build
// carries. It is separate from version() so the judgements below can be
// tested against inputs a test can actually produce -- a test binary's own
// build information describes the test binary, not a release, so it could
// never exercise the case that matters most.
//
// The judgements, in order:
//
//   - A module version is worth printing only when it names a RELEASE. Built
//     from a checkout, the toolchain synthesises a pseudo-version that merely
//     restates the revision and the dirty flag, so printing it beside them
//     says everything twice and buries the one word a reader wants, which is
//     that this is not a release at all. It is detected by asking whether it
//     contains the revision -- exactly the question being asked, does this
//     version add anything the revision has not already said, rather than a
//     guess at the toolchain's format that a later Go release could quietly
//     invalidate.
//
//   - vcs.modified is reported prominently, because a binary built from a
//     dirty tree IS NOT the commit it names, and a bug report quoting that
//     commit would send somebody to read source that was never compiled. It
//     is reported even when no revision came with it, so a stamped
//     modification can never pass for a clean build.
func formatVersion(mainVersion, revision, built, goVersion string, dirty bool) string {
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
