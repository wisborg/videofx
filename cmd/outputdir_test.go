package cmd

// Tests for prepareOutputDir and the --output-dir behaviour it backs: the
// directory is created up front and proven writable before any input file is
// read, so a run cannot render for minutes and then fail at the write. They
// live in their own file rather than in root_test.go because they exercise one
// function and its placement in runRoot, and because several of them chmod or
// chdir and are easier to reason about away from the flag-parsing tests.

import (
	"bytes"
	"errors"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"videofx/internal/logging"
)

func TestPrepareOutputDir_CreatesAMissingDirectoryIncludingParents(t *testing.T) {
	var logged bytes.Buffer
	log := logging.New(&logged, logging.LevelInfo).Named("videofx")

	dir := filepath.Join(t.TempDir(), "renders", "2026-08")
	if err := prepareOutputDir(log, dir); err != nil {
		t.Fatalf("prepareOutputDir(%q) = %v, want it to create the directory", dir, err)
	}

	info, err := os.Stat(dir)
	if err != nil {
		t.Fatalf("after prepareOutputDir, %q: %v -- returning nil without creating it is the silent no-op this function exists to remove", dir, err)
	}
	if !info.IsDir() {
		t.Fatalf("%q is not a directory", dir)
	}
	if !strings.Contains(logged.String(), "created output directory") {
		t.Errorf("creating the directory logged %q; a mistyped --output-dir now silently produces an empty directory instead of an error, so the creation must be visible", logged.String())
	}

	// The probe file must not outlive the probe.
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Errorf("the new directory holds %d entrie(s), want 0: the writability probe left its temp file behind", len(entries))
	}
}

// TestPrepareOutputDir_ExistingWritableDirectoryIsLeftAlone pins that the
// happy path is a no-op on the directory's contents and mode: it must not
// re-chmod a directory the user set up, and the write probe must clean up
// after itself (an --output-dir accumulating a dot-file per run would be a new
// bug shipped by the fix for an old one). Silence is asserted too -- "created"
// must not be logged for a directory that was already there.
func TestPrepareOutputDir_ExistingWritableDirectoryIsLeftAlone(t *testing.T) {
	var logged bytes.Buffer
	log := logging.New(&logged, logging.LevelInfo).Named("videofx")

	dir := t.TempDir()
	if err := os.Mkdir(filepath.Join(dir, "sub"), 0o750); err != nil {
		t.Fatal(err)
	}
	before, err := os.Stat(dir)
	if err != nil {
		t.Fatal(err)
	}

	if err := prepareOutputDir(log, dir); err != nil {
		t.Fatalf("prepareOutputDir(%q) = %v, want nil for an existing writable directory", dir, err)
	}

	after, err := os.Stat(dir)
	if err != nil {
		t.Fatal(err)
	}
	if before.Mode() != after.Mode() {
		t.Errorf("directory mode changed from %v to %v; an existing --output-dir must be used as it stands", before.Mode(), after.Mode())
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 || entries[0].Name() != "sub" {
		t.Errorf("directory now holds %v, want just [sub] -- the write probe must remove its temp file", entries)
	}
	if strings.Contains(logged.String(), "created") {
		t.Errorf("logged %q for a directory that already existed", logged.String())
	}
}

// TestPrepareOutputDir_ExistingFileIsRejectedWithADirectoryMessage pins the
// wording, not just the failure. os.MkdirAll on a path occupied by a file
// fails on its own, with the raw "mkdir /path/clip.mp4: not a directory" errno
// -- which reads like an internal error and never mentions the flag that
// caused it. The message has to name --output-dir and say what is wrong.
func TestPrepareOutputDir_ExistingFileIsRejectedWithADirectoryMessage(t *testing.T) {
	log := logging.New(&bytes.Buffer{}, logging.LevelInfo).Named("videofx")

	file := filepath.Join(t.TempDir(), "clip.mp4")
	if err := os.WriteFile(file, []byte("not a directory"), 0o644); err != nil {
		t.Fatal(err)
	}

	err := prepareOutputDir(log, file)
	if err == nil {
		t.Fatalf("prepareOutputDir(%q) = nil for an existing FILE; every output would then be written beside it under a name nobody asked for", file)
	}
	msg := err.Error()
	if !strings.Contains(msg, "--output-dir") || !strings.Contains(msg, "not a directory") {
		t.Errorf("error = %q, want it to name --output-dir and say the path is not a directory", msg)
	}
	if strings.Contains(msg, "mkdir ") {
		t.Errorf("error = %q, want the flag-level message rather than MkdirAll's raw syscall error", msg)
	}
	// The two assertions above are satisfied by ACCIDENT if the dedicated
	// !IsDir() branch is deleted: control then falls through to the writability
	// probe, whose os.CreateTemp on a file answers "open .../clip.mp4/
	// .videofx-write-check-4063743066: not a directory" -- which contains
	// "--output-dir" (from the probe's own wrapper), contains "not a directory"
	// (from the errno) and contains no "mkdir ". Measured: deleting the branch
	// left this test green. So the message has to be pinned to the branch that
	// actually understands the case: it says the path EXISTS and is not a
	// directory, it does not blame writability, and it does not leak the name
	// of an internal probe file at the user.
	if !strings.Contains(msg, "already exists") {
		t.Errorf("error = %q, want it to say the path already exists and is not a directory -- the writability probe's fallback message reaches here otherwise, and it explains the wrong thing", msg)
	}
	if strings.Contains(msg, "not writable") {
		t.Errorf("error = %q, want the existing-FILE message; \"not writable\" is what the write probe says, i.e. the dedicated branch was skipped", msg)
	}
	if strings.Contains(msg, "videofx-write-check") {
		t.Errorf("error = %q leaks the writability probe's temp filename; that is an internal detail and it means the case was diagnosed by the probe rather than by the stat", msg)
	}
	// The file itself must be untouched.
	if b, rerr := os.ReadFile(file); rerr != nil || string(b) != "not a directory" {
		t.Errorf("the existing file was modified (content %q, err %v)", b, rerr)
	}
}

// TestPrepareOutputDir_UnwritableDirectoryIsRejected is the case MkdirAll alone
// cannot catch: the directory exists, so MkdirAll returns nil, and the run
// proceeds to fail at write time exactly as it did before -- after the render.
// Only actually attempting a write finds it, which is why the probe exists.
//
// 0o500 is read+execute, no write: the directory can be listed and traversed
// (so a stat-based or mode-reading check that only asks "is it there" passes)
// but nothing can be created in it.
func TestPrepareOutputDir_UnwritableDirectoryIsRejected(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("running as root: the write permission bit does not bite")
	}
	log := logging.New(&bytes.Buffer{}, logging.LevelInfo).Named("videofx")

	dir := filepath.Join(t.TempDir(), "readonly")
	if err := os.Mkdir(dir, 0o500); err != nil {
		t.Fatal(err)
	}
	// t.TempDir's own cleanup needs to be able to remove this one.
	t.Cleanup(func() { _ = os.Chmod(dir, 0o700) })

	// The control: the directory really is one MkdirAll is happy with, so the
	// assertion below is about writability and not about a path that is simply
	// broken.
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("MkdirAll on the unwritable directory = %v; the fixture no longer isolates writability", err)
	}

	err := prepareOutputDir(log, dir)
	if err == nil {
		t.Fatalf("prepareOutputDir(%q) = nil for a directory nothing can be written to; the run would fail at write time, which for telemetry-hud is after the whole render", dir)
	}
	if !strings.Contains(err.Error(), "--output-dir") || !strings.Contains(err.Error(), "not writable") {
		t.Errorf("error = %q, want it to name --output-dir and say it is not writable", err)
	}
}

// TestPrepareOutputDir_UnsetTouchesNothing pins the default. --output-dir unset
// means "alongside each input file", which involves no directory of ours at
// all: an implementation that fell through to os.MkdirAll("") would fail every
// default run outright, and one that substituted "." would create and remove a
// probe file in whatever directory the user happened to be standing in.
//
// The working directory is a fresh empty temp dir, so any stray probe file
// (or directory) created by a "." fallback is visible in the listing even
// though a correctly-removed probe would not be.
func TestPrepareOutputDir_UnsetTouchesNothing(t *testing.T) {
	var logged bytes.Buffer
	log := logging.New(&logged, logging.LevelInfo).Named("videofx")

	dir := t.TempDir()
	t.Chdir(dir)

	if err := prepareOutputDir(log, ""); err != nil {
		t.Fatalf("prepareOutputDir(\"\") = %v, want nil: an unset --output-dir must not be a filesystem operation at all", err)
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Errorf("the working directory now holds %v, want nothing", entries)
	}
	if logged.Len() != 0 {
		t.Errorf("an unset --output-dir logged %q", logged.String())
	}
}

// TestRunRoot_UnwritableOutputDirFailsBeforeReadingAnyInput is the ordering
// half, and it is the point of putting the check in runRoot rather than in the
// processor: the invocation below is wrong twice over -- an unwritable
// --output-dir AND a nonexistent input -- and the error must be the
// --output-dir one, because that is the check that has to run before anything
// opens a file or spends a frame.
func TestRunRoot_UnwritableOutputDirFailsBeforeReadingAnyInput(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("running as root: the write permission bit does not bite")
	}
	origEffects, origFit, origOut := effectNames, fitPath, outputDir
	t.Cleanup(func() { effectNames, fitPath, outputDir = origEffects, origFit, origOut })

	dir := filepath.Join(t.TempDir(), "readonly")
	if err := os.Mkdir(dir, 0o500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(dir, 0o700) })

	err, logged := runRootCmd(t, "--effect", "telemetry", "--fit", "activity.fit",
		"--output-dir", dir, filepath.Join(t.TempDir(), "no-such-clip.mp4"))
	if err == nil {
		t.Fatalf("exited 0 with an unwritable --output-dir\n%s", logged)
	}
	if !strings.Contains(err.Error(), "--output-dir") {
		t.Errorf("error = %v, want the --output-dir failure; anything else means the check runs after work has already started", err)
	}
}

// TestRunRoot_OutputDirIsPreparedOncePerInvocation pins that the preparation is
// a property of the RUN, not of each output path. Two inputs, one missing
// --output-dir: the directory is created once and announced once. Moving the
// call into the per-file loop (or into naming.Resolve) would say it twice here
// and fifty times in a real batch.
//
// The run still fails -- the inputs do not exist -- which is fine and is not
// what is being asserted: prepareOutputDir runs before ValidateInputFiles, so
// its line is already in the log by then.
func TestRunRoot_OutputDirIsPreparedOncePerInvocation(t *testing.T) {
	origEffects, origFit, origOut := effectNames, fitPath, outputDir
	t.Cleanup(func() { effectNames, fitPath, outputDir = origEffects, origFit, origOut })

	tmp := t.TempDir()
	dir := filepath.Join(tmp, "renders")
	_, logged := runRootCmd(t, "--effect", "telemetry", "--fit", "activity.fit",
		"--output-dir", dir,
		filepath.Join(tmp, "a.mp4"), filepath.Join(tmp, "b.mp4"))

	if got := strings.Count(logged, "created output directory"); got != 1 {
		t.Errorf("two inputs announced the output directory %d time(s), want exactly 1\n%s", got, logged)
	}
	if info, err := os.Stat(dir); err != nil || !info.IsDir() {
		t.Errorf("stat %q: %v (isDir=%v), want the directory to have been created", dir, err, info != nil && info.IsDir())
	}
}

// TestRunRoot_OutputDirIsNotCreatedWhenAFreeCheckAlreadyFailed pins the
// position of the call, not just its behaviour: creating the directory is the
// one thing here with a side effect on the user's filesystem, so it must come
// after every check that costs nothing.
//
// The invocation below fails in configureEffect (an unknown --edge-mode), which
// used to run AFTER prepareOutputDir -- so a mistyped flag left an empty
// directory behind, which is precisely the "silently created a directory you
// did not want" case the run's own info line exists to soften. The same
// applies to an effect whose external tool is missing.
func TestRunRoot_OutputDirIsNotCreatedWhenAFreeCheckAlreadyFailed(t *testing.T) {
	origEffects, origEdge, origOut := effectNames, edgeMode, outputDir
	t.Cleanup(func() { effectNames, edgeMode, outputDir = origEffects, origEdge, origOut })

	dir := filepath.Join(t.TempDir(), "renders")
	err, logged := runRootCmd(t, "--effect", "gocv-stabilizer", "--edge-mode", "no-such-mode",
		"--output-dir", dir, filepath.Join(t.TempDir(), "clip.mp4"))
	if err == nil {
		t.Fatalf("an unknown --edge-mode exited 0\n%s", logged)
	}
	if _, statErr := os.Stat(dir); !errors.Is(statErr, fs.ErrNotExist) {
		t.Errorf("stat %q = %v, want it never to have been created: the run failed on a check that costs nothing, so it should leave nothing behind", dir, statErr)
	}
}

// TestPrepareOutputDir_UnstattableePathIsReportedNotSilentlyCreated covers the
// Stat error that is NOT "missing". A dangling symlink is the reachable one: it
// exists as a name, so MkdirAll fails with a bare "mkdir ...: file exists" --
// the raw-errno shape this function refuses to hand the user for the
// existing-file case, and no more informative here.
func TestPrepareOutputDir_UnstattablePathIsReportedNotSilentlyCreated(t *testing.T) {
	log := logging.New(&bytes.Buffer{}, logging.LevelInfo).Named("videofx")

	dir := t.TempDir()
	link := filepath.Join(dir, "renders")
	if err := os.Symlink(filepath.Join(dir, "no-such-target"), link); err != nil {
		t.Fatal(err)
	}

	err := prepareOutputDir(log, link)
	if err == nil {
		t.Fatalf("prepareOutputDir(%q) = nil for a dangling symlink", link)
	}
	if !strings.Contains(err.Error(), "--output-dir") {
		t.Errorf("error = %q, want it to name --output-dir", err)
	}
	if strings.Contains(err.Error(), "mkdir ") {
		t.Errorf("error = %q, want a flag-level message rather than MkdirAll's raw syscall error", err)
	}

	// The control, and a real use: a symlink to a directory that EXISTS is a
	// perfectly good --output-dir (an external drive linked into a project
	// folder, say). Rejecting symlinks as a class would break it, so the
	// distinction has to be the target, not the link.
	target := filepath.Join(dir, "real")
	if err := os.Mkdir(target, 0o755); err != nil {
		t.Fatal(err)
	}
	good := filepath.Join(dir, "linked")
	if err := os.Symlink(target, good); err != nil {
		t.Fatal(err)
	}
	if err := prepareOutputDir(log, good); err != nil {
		t.Errorf("prepareOutputDir(%q) = %v for a symlink to an existing directory, want nil", good, err)
	}

	// The other non-missing Stat failure: a path whose PARENT cannot be
	// traversed answers EACCES, not ENOENT. Treating every Stat error as
	// "missing" sends it to MkdirAll, which reports the same permission problem
	// as a raw "mkdir ...: permission denied".
	if os.Geteuid() == 0 {
		return // root traverses a 0o000 directory, so there is nothing to observe
	}
	locked := filepath.Join(dir, "locked")
	if err := os.Mkdir(locked, 0o000); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(locked, 0o700) })
	err = prepareOutputDir(log, filepath.Join(locked, "renders"))
	if err == nil {
		t.Fatal("prepareOutputDir = nil for a path inside an untraversable directory")
	}
	if !strings.Contains(err.Error(), "--output-dir") || strings.Contains(err.Error(), "mkdir ") {
		t.Errorf("error = %q, want a flag-level message rather than MkdirAll's raw syscall error", err)
	}
}

func TestPrepareOutputDir_UnsetDoesNotProbeTheWorkingDirectory(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("running as root: the write permission bit does not bite")
	}
	var logged bytes.Buffer
	log := logging.New(&logged, logging.LevelInfo).Named("videofx")

	dir := t.TempDir()
	t.Chdir(dir)
	// Registered after t.Chdir, so it runs BEFORE the chdir-back and before
	// t.TempDir's removal (cleanups are LIFO): the directory has to be writable
	// again for either of those to succeed.
	t.Cleanup(func() { _ = os.Chmod(dir, 0o700) })
	if err := os.Chmod(dir, 0o500); err != nil {
		t.Fatal(err)
	}

	// The control: the working directory really is one nothing can be created
	// in, so a nil below is about --output-dir being unset and not about a
	// probe that would have succeeded anyway.
	if f, err := os.CreateTemp(".", "control-*"); err == nil {
		name := f.Name()
		_ = f.Close()
		_ = os.Remove(name)
		t.Fatalf("the working directory is still writable; the fixture no longer isolates anything")
	}

	if err := prepareOutputDir(log, ""); err != nil {
		t.Fatalf("prepareOutputDir(\"\") = %v in a read-only working directory, want nil: with --output-dir unset every output goes beside its own input, so the current directory is never written to and must never be tested", err)
	}
	if logged.Len() != 0 {
		t.Errorf("an unset --output-dir logged %q", logged.String())
	}
}

// TestRunRoot_OutputDirCreatedIsTheDirectoryTheOutputLandsIn closes the loop
// that every other --output-dir test leaves open: they prove the directory is
// created, or that the run fails early, but not that the created directory is
// the one the run then writes into.
//
// That is a real gap rather than a pedantic one. prepareOutputDir and
// naming.Resolve reach the same path independently -- one from os.MkdirAll, the
// other from filepath.Join(outputDir, ...) -- so a change to either (trailing
// separator, symlink resolution, an absolute/relative normalisation) can leave
// prepareOutputDir creating and probing directory A while the render is written
// into directory B. The run would still exit 0, and B is created implicitly by
// nobody, so the failure would be an ffmpeg write error at the END of the work:
// exactly what this feature exists to prevent.
//
// rotate is the effect because it is a stream copy -- the whole run is well
// under a second -- and the assertion is only about where the file lands.
//
// The relative subtest is not decoration. Every fixture path in this file comes
// from t.TempDir() and is therefore ABSOLUTE, so a --output-dir handled
// correctly only when absolute would pass everything here. "--output-dir
// renders" typed against the current directory is the commoner invocation of
// the two.
func TestRunRoot_OutputDirCreatedIsTheDirectoryTheOutputLandsIn(t *testing.T) {
	if _, err := exec.LookPath("ffmpeg"); err != nil {
		t.Skip("ffmpeg not on PATH")
	}
	origEffects, origRotate, origOut := effectNames, rotateDeg, outputDir
	t.Cleanup(func() { effectNames, rotateDeg, outputDir = origEffects, origRotate, origOut })

	for _, c := range []struct {
		name string
		// dirFor returns the --output-dir to pass and the absolute path it
		// should resolve to, given a scratch root the test owns.
		dirFor func(t *testing.T, root string) (flag, abs string)
	}{
		{
			name: "absolute, two levels missing",
			dirFor: func(t *testing.T, root string) (string, string) {
				abs := filepath.Join(root, "renders", "2026-08")
				return abs, abs
			},
		},
		{
			name: "relative to the working directory",
			dirFor: func(t *testing.T, root string) (string, string) {
				t.Chdir(root)
				return filepath.Join("renders", "2026-08"), filepath.Join(root, "renders", "2026-08")
			},
		},
	} {
		t.Run(c.name, func(t *testing.T) {
			srcDir := t.TempDir()
			src := filepath.Join(srcDir, "clip.mp4")
			genClipAt(t, src, 1, time.Date(2026, 8, 1, 9, 0, 0, 0, time.UTC))

			flag, abs := c.dirFor(t, t.TempDir())
			err, logged := runRootCmd(t, "--effect", "rotate", "--rotate", "90",
				"--output-dir", flag, src)
			if err != nil {
				t.Fatalf("videofx --output-dir %s = %v\n%s", flag, err, logged)
			}

			entries, readErr := os.ReadDir(abs)
			if readErr != nil {
				t.Fatalf("reading %q: %v -- the run exited 0, so the output went somewhere else\n%s", abs, readErr, logged)
			}
			if len(entries) != 1 {
				t.Fatalf("%q holds %v, want exactly the one rotated clip\n%s", abs, entries, logged)
			}
			// And nothing was left beside the input, which is where the output
			// goes when --output-dir is not honoured at all.
			srcEntries, readErr := os.ReadDir(srcDir)
			if readErr != nil {
				t.Fatal(readErr)
			}
			if len(srcEntries) != 1 {
				t.Errorf("the input's own directory holds %v, want just the source: --output-dir did not redirect the write", srcEntries)
			}
		})
	}
}

// TestRunRoot_TelemetryHUDSidecarsSurviveValidation is the end-to-end
// counterpart to TestRunRoot_MidChainSidecarIsRejectedBeforeAnyWork: the same
// wiring, asked to say nothing.
//
// `--effect telemetry-hud --gpx --srt-sidecar` is a legitimate and common
// invocation. impliedEffects turns it into a TWO-effect chain whose first
// member is named "telemetry-hud", so any identification of the telemetry pass
// by name prefix finds a non-final effect and rejects the run. The unit test
// above pins the lookup; this pins that nothing between the flags and the
// validator reintroduces the problem.
//
// The input does not exist, so the run fails -- on ValidateInputFiles, which is
// the point: the sidecar check sits ahead of it, so if it were going to object
// it would have objected first.
