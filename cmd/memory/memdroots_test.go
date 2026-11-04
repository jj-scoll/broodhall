package main

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"
)

func mkdirs(t *testing.T, dirs ...string) {
	t.Helper()
	for _, d := range dirs {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
}

// One daemon serves every account: whichever account starts it, it tails the projects dir of
// each ~/.claude* config dir, so one database holds them all.
func TestMemdRootsFindsEveryAccount(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("MEMORY_CONFIG_DIRS", "")
	mkdirs(t,
		filepath.Join(home, ".claude", "projects"),
		filepath.Join(home, ".claude-work", "projects"),
		filepath.Join(home, ".claude-memd"), // no projects dir: not a config dir
	)
	if err := os.WriteFile(filepath.Join(home, ".claude.json"), []byte("{}"), 0o644); err != nil {
		t.Fatal(err)
	}
	want := map[string]string{
		filepath.Join(home, ".claude", "projects"):      "claude",
		filepath.Join(home, ".claude-work", "projects"): "claude-work",
	}
	for _, cfg := range []string{"", filepath.Join(home, ".claude"), filepath.Join(home, ".claude-work")} {
		t.Setenv("CLAUDE_CONFIG_DIR", cfg)
		if got := defaultMemdRoots(); !reflect.DeepEqual(got, want) {
			t.Errorf("CLAUDE_CONFIG_DIR=%q: roots = %v, want %v", cfg, got, want)
		}
	}
}

// A CLAUDE_CONFIG_DIR outside ~/.claude* is tailed too, alongside the ones found in HOME.
func TestMemdRootsKeepsConfigDirOutsideHome(t *testing.T) {
	home := t.TempDir()
	other := filepath.Join(t.TempDir(), "cfg")
	t.Setenv("HOME", home)
	t.Setenv("MEMORY_CONFIG_DIRS", "")
	t.Setenv("CLAUDE_CONFIG_DIR", other)
	mkdirs(t, filepath.Join(home, ".claude", "projects"), filepath.Join(other, "projects"))
	want := map[string]string{
		filepath.Join(home, ".claude", "projects"): "claude",
		filepath.Join(other, "projects"):           "cfg",
	}
	if got := defaultMemdRoots(); !reflect.DeepEqual(got, want) {
		t.Errorf("roots = %v, want %v", got, want)
	}
}

// MEMORY_CONFIG_DIRS replaces the search: exactly the listed config dirs, existing or not
// (one created later is tailed once it appears).
func TestMemdRootsEnvOverride(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	mkdirs(t, filepath.Join(home, ".claude", "projects"))
	a, b := filepath.Join(home, "a"), filepath.Join(home, "b")
	sep := string(os.PathListSeparator)
	// A relative entry would resolve against the daemon's cwd: ignored, like the XDG vars.
	t.Setenv("MEMORY_CONFIG_DIRS", a+sep+"rel/dir"+sep+"~/.claude"+sep+b)
	want := map[string]string{filepath.Join(a, "projects"): "a", filepath.Join(b, "projects"): "b"}
	if got := defaultMemdRoots(); !reflect.DeepEqual(got, want) {
		t.Errorf("roots = %v, want %v", got, want)
	}
}

// A config dir reached through a symlink keeps one spelling whichever account starts the
// daemon, so its stored offsets and backfill cutoff keep matching across restarts.
func TestMemdRootsSymlinkSpellingIsStable(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("MEMORY_CONFIG_DIRS", "")
	real := filepath.Join(home, "data", "work")
	mkdirs(t, filepath.Join(home, ".claude", "projects"), filepath.Join(real, "projects"))
	if err := os.Symlink(real, filepath.Join(home, ".claude-work")); err != nil {
		t.Fatal(err)
	}
	want := map[string]string{
		filepath.Join(home, ".claude", "projects"):      "claude",
		filepath.Join(home, ".claude-work", "projects"): "claude-work",
	}
	for _, cfg := range []string{"", real, filepath.Join(home, ".claude-work")} {
		t.Setenv("CLAUDE_CONFIG_DIR", cfg)
		if got := defaultMemdRoots(); !reflect.DeepEqual(got, want) {
			t.Errorf("CLAUDE_CONFIG_DIR=%q: roots = %v, want %v", cfg, got, want)
		}
	}
}

// The cutoff reads transcript_offsets.updated_at back as UTC. It is written by DuckDB's
// current_timestamp, so a local time zone must not shift it.
func TestMemdOffsetUpdatedAtIsUTC(t *testing.T) {
	t.Setenv("TZ", "America/New_York")
	e := newTailEnv(t, "")
	tailAppend(t, e.path("sess-u.jsonl"), tailPair("sess-u", "tu", time.Now().UTC().Format("2006-01-02T15:04:05.000Z")))
	tl := newMemdTailer(e.db, map[string]string{e.root: "acct"}, time.Now().Add(-time.Minute))
	tailScan(t, tl)
	var at time.Time
	if err := e.db.QueryRow("SELECT updated_at FROM transcript_offsets").Scan(&at); err != nil {
		t.Fatal(err)
	}
	if d := time.Since(at); d < -time.Minute || d > time.Minute {
		t.Errorf("updated_at = %v, %v from now; want within a minute (UTC)", at, d)
	}
}

// setCovered stamps every stored offset under the env's root as last written at ts, as if an
// earlier daemon had last tailed that root then.
func setCovered(t *testing.T, e *tailEnv, ts time.Time) {
	t.Helper()
	if _, err := e.db.Exec("UPDATE transcript_offsets SET updated_at = ?", memdTS(ts)); err != nil {
		t.Fatal(err)
	}
}

// Another account's daemon held the socket for a while, so this root went untailed. When a
// daemon tails it again, a transcript it never saw is read from the last time the root was
// covered, not from the new start: the session's early prompts are kept.
func TestMemdTailBackfillsRootSinceLastCovered(t *testing.T) {
	e := newTailEnv(t, "")
	tailAppend(t, e.path("sess-old.jsonl"), tailPair("sess-old", "told", tailTS(9, 0, 0)))
	tailScan(t, e.tailer())
	setCovered(t, e, tailStart.Add(10*time.Hour))

	fresh := e.path("sess-new.jsonl")
	tailAppend(t, fresh, tailPrompt("sess-new", "before cover", tailTS(9, 30, 0), "cli")+
		tailPrompt("sess-new", "in gap", tailTS(10, 30, 0), "cli")+tailPair("sess-new", "tgap", tailTS(10, 30, 1)))
	gap := tailStart.Add(10*time.Hour + 31*time.Minute) // last written before the new start
	if err := os.Chtimes(fresh, gap, gap); err != nil {
		t.Fatal(err)
	}

	restarted := newMemdTailer(e.db, map[string]string{e.root: "acct"}, tailStart.Add(11*time.Hour))
	tailScan(t, restarted)
	for q, want := range map[string]int{
		"SELECT count(*) FROM prompts WHERE text='in gap'":        1,
		"SELECT count(*) FROM tool_uses WHERE tool_use_id='tgap'": 1,
		"SELECT count(*) FROM prompts WHERE text='before cover'":  0,
	} {
		if got := tailCount(t, e.db, q); got != want {
			t.Errorf("%s = %d, want %d", q, got, want)
		}
	}
}

// The backfill reaches back at most memdBackfillMax before the start, so re-enabling memory
// after weeks does not queue weeks of sessions for the observer.
func TestMemdTailBackfillIsCapped(t *testing.T) {
	e := newTailEnv(t, "")
	tailAppend(t, e.path("sess-old.jsonl"), tailPair("sess-old", "told", tailTS(9, 0, 0)))
	tailScan(t, e.tailer())
	start := tailStart.Add(72 * time.Hour)
	setCovered(t, e, start.Add(-48*time.Hour))

	fresh := e.path("sess-cap.jsonl")
	tooOld := start.Add(-memdBackfillMax - time.Hour).Format("2006-01-02T15:04:05.000Z")
	recent := start.Add(-time.Hour).Format("2006-01-02T15:04:05.000Z")
	tailAppend(t, fresh, tailPrompt("sess-cap", "too old", tooOld, "cli")+tailPrompt("sess-cap", "recent", recent, "cli"))
	mt := start.Add(-time.Hour)
	if err := os.Chtimes(fresh, mt, mt); err != nil {
		t.Fatal(err)
	}

	tailScan(t, newMemdTailer(e.db, map[string]string{e.root: "acct"}, start))
	for q, want := range map[string]int{
		"SELECT count(*) FROM prompts WHERE text='recent'":  1,
		"SELECT count(*) FROM prompts WHERE text='too old'": 0,
	} {
		if got := tailCount(t, e.db, q); got != want {
			t.Errorf("%s = %d, want %d", q, got, want)
		}
	}
}

// Each root has its own cutoff: a root covered recently keeps the start as its cutoff while
// another root, untailed for hours, is backfilled.
func TestMemdTailCutoffIsPerRoot(t *testing.T) {
	e := newTailEnv(t, "")
	other := t.TempDir()
	otherDir := filepath.Join(other, "projects", "-work-demo")
	mkdirs(t, otherDir)
	tailAppend(t, e.path("sess-a.jsonl"), tailPair("sess-a", "ta", tailTS(9, 0, 0)))
	tailAppend(t, filepath.Join(otherDir, "sess-b.jsonl"), tailPair("sess-b", "tb", tailTS(9, 0, 0)))
	both := map[string]string{e.root: "a", other: "b"}
	tailScan(t, newMemdTailer(e.db, both, tailStart))
	start := tailStart.Add(11 * time.Hour)
	if _, err := e.db.Exec("UPDATE transcript_offsets SET updated_at = CASE WHEN path LIKE ? THEN ?::TIMESTAMP ELSE ?::TIMESTAMP END",
		e.root+"/%", memdTS(tailStart.Add(10*time.Hour)), memdTS(start)); err != nil {
		t.Fatal(err)
	}

	gap := tailStart.Add(10*time.Hour + 31*time.Minute)
	for _, p := range []string{e.path("new-a.jsonl"), filepath.Join(otherDir, "new-b.jsonl")} {
		tailAppend(t, p, tailPrompt(filepath.Base(p), "gap "+filepath.Base(p), tailTS(10, 30, 0), "cli"))
		if err := os.Chtimes(p, gap, gap); err != nil {
			t.Fatal(err)
		}
	}
	tailScan(t, newMemdTailer(e.db, both, start))
	for q, want := range map[string]int{
		"SELECT count(*) FROM prompts WHERE text='gap new-a.jsonl'": 1,
		"SELECT count(*) FROM prompts WHERE text='gap new-b.jsonl'": 0,
	} {
		if got := tailCount(t, e.db, q); got != want {
			t.Errorf("%s = %d, want %d", q, got, want)
		}
	}
}
