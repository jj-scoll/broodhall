package main

import (
	"database/sql"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

// All fixtures are synthetic.

func writeF(t *testing.T, p, body string) string {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestFileFingerprintMatchesGitHashObject(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("no git")
	}
	dir := t.TempDir()
	p := writeF(t, filepath.Join(dir, "a.txt"), "hello\nworld\n")
	want, err := exec.Command("git", "hash-object", "--no-filters", p).Output()
	if err != nil {
		t.Fatal(err)
	}
	got := fileFingerprint(p, nil)
	if got != "blob:"+strings.TrimSpace(string(want)) {
		t.Fatalf("fingerprint = %q, git hash-object = %q", got, want)
	}
}

func TestFileFingerprintSkips(t *testing.T) {
	dir := t.TempDir()
	if fp := fileFingerprint(filepath.Join(dir, "missing"), nil); fp != "" {
		t.Errorf("missing file: %q", fp)
	}
	if fp := fileFingerprint(dir, nil); fp != "" {
		t.Errorf("directory: %q", fp)
	}
	big := filepath.Join(dir, "big.bin")
	f, _ := os.Create(big)
	f.Truncate(staleMaxBytes + 1)
	f.Close()
	if fp := fileFingerprint(big, nil); fp != "" {
		t.Errorf("over the size limit: %q", fp)
	}
	if err := os.Symlink(filepath.Join(dir, "missing"), filepath.Join(dir, "dangling")); err == nil {
		if fp := fileFingerprint(filepath.Join(dir, "dangling"), nil); fp != "" {
			t.Errorf("dangling symlink: %q", fp)
		}
	}
	// A protected secret file is never read, so never fingerprinted.
	home := t.TempDir()
	t.Setenv("HOME", home)
	secret := writeF(t, filepath.Join(home, ".aws", "credentials"), "aws_secret_access_key=x")
	if fp := fileFingerprint(secret, newStaleChecker().guard); fp != "" {
		t.Errorf("secret file fingerprinted: %q", fp)
	}
}

// oldFilesDDL is observation_files as origin/main creates it.
const oldFilesDDL = `CREATE TABLE observation_files (
	observation_id BIGINT NOT NULL,
	path           VARCHAR NOT NULL,
	kind           VARCHAR NOT NULL CHECK (kind IN ('read','modified'))
)`

func TestMigrateAddsFingerprintColumnsInPlace(t *testing.T) {
	db, _ := openTestMemd(t)
	for _, q := range []string{`DROP TABLE observation_files`, oldFilesDDL,
		`INSERT INTO observation_files VALUES (7, 'old.go', 'read'), (7, 'old2.go', 'modified')`} {
		if _, err := db.Exec(q); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
	}
	for i := 0; i < 2; i++ { // twice: the migration must be idempotent
		if err := memdMigrate(db); err != nil {
			t.Fatalf("migrate %d: %v", i, err)
		}
	}
	var n, nulls int
	if err := db.QueryRow(`SELECT count(*), count(*) FILTER (WHERE fingerprint IS NULL AND fp_path IS NULL) FROM observation_files`).Scan(&n, &nulls); err != nil {
		t.Fatal(err)
	}
	if n != 2 || nulls != 2 {
		t.Fatalf("rows = %d, with null fingerprint = %d; want 2, 2", n, nulls)
	}
	if _, err := db.Exec(`INSERT INTO observation_files (observation_id, path, kind, fingerprint, fp_path) VALUES (8, 'n.go', 'read', 'blob:x', '/n.go')`); err != nil {
		t.Fatal(err)
	}
}

func TestWorkerStoresFingerprints(t *testing.T) {
	fastWorkers(t)
	db, _ := openTestMemd(t)
	dir := t.TempDir()
	abs := writeF(t, filepath.Join(dir, "src", "parser.go"), "package p\n")
	if _, err := db.Exec(`INSERT INTO sessions (session_id, project, cwd) VALUES ('sess', 'proj-x', ?)`, dir); err != nil {
		t.Fatal(err)
	}
	in := fmt.Sprintf(`{"file_path":%q,"new_string":"x"}`, abs)
	id := qAdd(t, db, "sess", "tu1", "Edit", in)
	if _, err := db.Exec(`UPDATE tool_uses SET output = ?`, "The file "+abs+" has been updated. src/parser.go and src/gone.go too."); err != nil {
		t.Fatal(err)
	}
	m := goodObs()
	m["files_modified"] = []string{abs}
	m["files_read"] = []string{"src/parser.go", "src/gone.go"} // relative: resolved against the session cwd
	newScripted(t, func(int, string) fakeReply { return result(t, m, `{"input_tokens":1,"output_tokens":1}`) })
	startWorkers(t, db, 1)
	waitFor(t, "done", func() bool { st, _, _, _ := qState(t, db, id); return st == "done" })

	want := fileFingerprint(abs, nil)
	rows, err := db.Query(`SELECT path, coalesce(fingerprint,''), coalesce(fp_path,'') FROM observation_files ORDER BY path`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	got := map[string][2]string{}
	for rows.Next() {
		var p, fp, fpp string
		rows.Scan(&p, &fp, &fpp)
		got[p] = [2]string{fp, fpp}
	}
	if g := got[abs]; g[0] != want || g[1] != abs || want == "" {
		t.Errorf("absolute file: %v want fp %q at %q", g, want, abs)
	}
	if g := got["src/parser.go"]; g[0] != want || g[1] != abs {
		t.Errorf("relative file: %v want fp %q at %q", g, want, abs)
	}
	if g, ok := got["src/gone.go"]; !ok || g[0] != "" || g[1] != "" {
		t.Errorf("unreadable file must be stored without a fingerprint: %v ok=%v", g, ok)
	}
}

func fpRow(t *testing.T, db *sql.DB, id int, path, kind, fpPath string) {
	t.Helper()
	fp := fileFingerprint(fpPath, nil)
	if _, err := db.Exec(`INSERT INTO observation_files VALUES (?,?,?,?,?)`, id, path, kind, nullStr(fp), nullStr(fpPath)); err != nil {
		t.Fatal(err)
	}
}

func nullStr(s string) any {
	if s == "" {
		return nil
	}
	return s
}

func TestStaleStates(t *testing.T) {
	dir := t.TempDir()
	same := writeF(t, filepath.Join(dir, "same.go"), "a")
	chg := writeF(t, filepath.Join(dir, "chg.go"), "a")
	del := writeF(t, filepath.Join(dir, "del.go"), "a")
	ren := writeF(t, filepath.Join(dir, "ren.go"), "a")
	fps := map[string]string{}
	for _, p := range []string{same, chg, del, ren} {
		fps[p] = fileFingerprint(p, nil)
	}
	writeF(t, chg, "b")
	os.Remove(del)
	os.Rename(ren, ren+".new")
	big := filepath.Join(dir, "big")
	f, _ := os.Create(big)
	f.Truncate(staleMaxBytes + 10)
	f.Close()

	c := newStaleChecker()
	cases := []struct {
		name, path, fp string
		want           staleState
	}{
		{"unchanged", same, fps[same], fileFresh},
		{"changed", chg, fps[chg], fileChanged},
		{"deleted", del, fps[del], fileGone},
		{"renamed", ren, fps[ren], fileGone},
		{"no fingerprint", chg, "", fileUnknown},
		{"other machine", "/nonexistent-host-dir-xyz/proj/a.go", "blob:abc", fileUnknown},
		{"grown past limit", big, "blob:abc", fileUnknown},
	}
	for _, tc := range cases {
		if got := c.state(tc.path, tc.fp); got != tc.want {
			t.Errorf("%s: state = %v, want %v", tc.name, got, tc.want)
		}
	}
}

func TestStaleCheckerCapAndCache(t *testing.T) {
	dir := t.TempDir()
	c := newStaleChecker()
	c.maxHash = 3
	var ps []string
	for i := 0; i < 6; i++ {
		p := writeF(t, filepath.Join(dir, fmt.Sprintf("f%d", i)), "x")
		ps = append(ps, p)
	}
	for _, p := range ps {
		c.state(p, "blob:other") // all differ
	}
	if c.hashed != 3 {
		t.Fatalf("hashed %d files, cap is 3", c.hashed)
	}
	// Same path again: served from the cache, not hashed again.
	before := c.hashed
	c.state(ps[0], "blob:other")
	if c.hashed != before {
		t.Fatal("second lookup of the same path hashed again")
	}
	if c.state(ps[5], "blob:other") != fileUnknown {
		t.Fatal("past the cap a file must be unknown, not changed")
	}
}

func staleSeed(t *testing.T) (*sql.DB, string) {
	t.Helper()
	db := ctxDB(t)
	dir := t.TempDir()
	chg := writeF(t, filepath.Join(dir, "chg.go"), "v1")
	ok := writeF(t, filepath.Join(dir, "ok.go"), "v1")
	del := writeF(t, filepath.Join(dir, "del.go"), "v1")
	ctxObs(t, db, 1, "p", "Changed note", "uniqueword narrative", ctxNow.Add(-3*time.Hour))
	ctxObs(t, db, 2, "p", "Fine note", "uniqueword narrative", ctxNow.Add(-2*time.Hour))
	ctxObs(t, db, 3, "p", "Deleted note", "uniqueword narrative", ctxNow.Add(-1*time.Hour))
	ctxObs(t, db, 4, "p", "Legacy note", "uniqueword narrative", ctxNow.Add(-30*time.Minute))
	fpRow(t, db, 1, chg, "modified", chg)
	fpRow(t, db, 2, ok, "modified", ok)
	fpRow(t, db, 3, del, "read", del)
	ctxExec(t, db, `INSERT INTO observation_files (observation_id, path, kind) VALUES (4, ?, 'read')`, writeF(t, filepath.Join(dir, "legacy.go"), "v1")) // old row, no fingerprint
	writeF(t, chg, "v2")
	os.Remove(del)
	return db, dir
}

func TestContextMarksStaleNotes(t *testing.T) {
	db, _ := staleSeed(t)
	got := buildContext(db, "p", ctxNow)
	for _, tc := range []struct{ title, mark string }{
		{"Changed note", "(file changed since)"},
		{"Deleted note", "(file removed since)"},
	} {
		if !strings.Contains(got, tc.title+" "+tc.mark) {
			t.Errorf("%s not marked %s:\n%s", tc.title, tc.mark, got)
		}
	}
	for _, title := range []string{"Fine note", "Legacy note"} {
		if strings.Contains(got, title+" (file") {
			t.Errorf("%s must stay unmarked:\n%s", title, got)
		}
	}
}

func TestGetObservationsMarksStaleNotes(t *testing.T) {
	db, _ := staleSeed(t)
	out, err := memGetObservations(db, map[string]any{"ids": []any{1.0, 2.0, 3.0, 4.0}})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "Changed note (p) (file changed since)") && !strings.Contains(out, "Changed note (file changed since)") {
		t.Errorf("changed note not marked:\n%s", out)
	}
	if !strings.Contains(out, "(file removed since)") {
		t.Errorf("deleted note not marked:\n%s", out)
	}
	if strings.Count(out, "(file ") != 2 {
		t.Errorf("want exactly 2 marks:\n%s", out)
	}
}

func TestSearchMarksAndDemotesStaleNotes(t *testing.T) {
	db, _ := staleSeed(t)
	out, err := memSearch(db, map[string]any{"query": "uniqueword"})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "Changed note (file changed since)") || !strings.Contains(out, "Deleted note (file removed since)") {
		t.Errorf("stale notes not marked:\n%s", out)
	}
	if strings.Contains(out, "Fine note (file") || strings.Contains(out, "Legacy note (file") {
		t.Errorf("fresh notes marked:\n%s", out)
	}
	// Ranking: every fresh note is listed before every stale one.
	last := func(t string) int { return strings.Index(out, t) }
	for _, fresh := range []string{"Fine note", "Legacy note"} {
		for _, stale := range []string{"Changed note", "Deleted note"} {
			if last(fresh) > last(stale) {
				t.Errorf("%s listed after %s:\n%s", fresh, stale, out)
			}
		}
	}
}

func TestStaleCheckIsBoundedAtScale(t *testing.T) {
	db := ctxDB(t)
	dir := t.TempDir()
	// 5000 notes, each on its own existing file whose content changed.
	for i := 1; i <= 5000; i++ {
		os.WriteFile(filepath.Join(dir, fmt.Sprintf("f%d.go", i)), []byte("v2"), 0o644)
	}
	ctxExec(t, db, `INSERT INTO observations (id, project, session_id, type, title, narrative, source, created_at)
		SELECT i, 'p', 's', 'discovery', 'note ' || i, 'n', 'live', CAST(? AS TIMESTAMP) - to_minutes(5001 - i::INTEGER) FROM range(1, 5001) t(i)`,
		ctxNow.Format("2006-01-02 15:04:05"))
	ctxExec(t, db, `INSERT INTO observation_files SELECT i, ? || '/f' || i || '.go', 'read', 'blob:old', ? || '/f' || i || '.go' FROM range(1, 5001) t(i)`, dir, dir)
	start := time.Now()
	got := buildContext(db, "p", ctxNow)
	el := time.Since(start)
	t.Logf("buildContext with 5000 notes: %v", el)
	if !strings.Contains(got, "(file changed since)") {
		t.Fatalf("recent notes not marked:\n%s", got)
	}
	if el > 2*time.Second {
		t.Fatalf("session-start context took %v", el)
	}
}

// within runs f and fails the test if it does not return in time (a FIFO read blocks forever).
func within[T any](t *testing.T, what string, f func() T) T {
	t.Helper()
	done := make(chan T, 1)
	go func() { done <- f() }()
	select {
	case v := <-done:
		return v
	case <-time.After(3 * time.Second):
		t.Fatalf("%s blocked for more than 3s", what)
		panic("unreachable")
	}
}

func TestFifoNeverBlocks(t *testing.T) {
	dir := t.TempDir()
	ff := filepath.Join(dir, "pipe")
	if err := syscall.Mkfifo(ff, 0o644); err != nil {
		t.Skip("no mkfifo")
	}
	if fp := within(t, "fileFingerprint", func() string { return fileFingerprint(ff, nil) }); fp != "" {
		t.Errorf("fifo fingerprinted: %q", fp)
	}
	if m := within(t, "recordFingerprints", func() map[string][2]string { return recordFingerprints(dir, []string{"pipe"}) }); len(m) != 0 {
		t.Errorf("fifo recorded: %v", m)
	}
	if st := within(t, "state", func() staleState { return newStaleChecker().state(ff, "blob:x") }); st != fileUnknown {
		t.Errorf("fifo state = %v, want unknown", st)
	}
}

func TestNonRegularFilesAreUnknown(t *testing.T) {
	dir := t.TempDir()
	d := filepath.Join(dir, "was-a-file")
	writeF(t, d, "x")
	fp := fileFingerprint(d, nil)
	os.Remove(d)
	os.Mkdir(d, 0o755)
	if st := newStaleChecker().state(d, fp); st != fileUnknown {
		t.Errorf("file replaced by a directory: state = %v, want unknown", st)
	}
	g := writeF(t, filepath.Join(dir, "grow"), "x")
	gfp := fileFingerprint(g, nil)
	os.WriteFile(g, make([]byte, staleMaxBytes+5), 0o644)
	if st := newStaleChecker().state(g, gfp); st != fileUnknown {
		t.Errorf("file grown past the limit: state = %v, want unknown", st)
	}
}

func TestSecretFilesAreNeverReadOrReportedGone(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", "")
	sec := writeF(t, filepath.Join(home, ".aws", "credentials"), "k")
	ln := filepath.Join(t.TempDir(), "innocent.txt")
	os.Symlink(sec, ln)
	c := newStaleChecker()
	if st := c.state(sec, "blob:x"); st != fileUnknown {
		t.Errorf("secret file: state = %v, want unknown", st)
	}
	if c.hashed != 0 {
		t.Errorf("secret file was hashed")
	}
	os.Remove(sec)
	if st := newStaleChecker().state(sec, "blob:x"); st != fileUnknown {
		t.Errorf("deleted secret file: state = %v, want unknown", st)
	}
}

func TestSearchWithoutQueryIsNotDemoted(t *testing.T) {
	db, _ := staleSeed(t)
	out, err := memSearch(db, map[string]any{})
	if err != nil {
		t.Fatal(err)
	}
	// Newest first: Deleted note (stale, 1h old) comes before Fine note (fresh, 2h old).
	if strings.Index(out, "Deleted note") > strings.Index(out, "Fine note") {
		t.Errorf("a query-less listing must stay newest first:\n%s", out)
	}
}

func TestTimelineMarksStaleNotes(t *testing.T) {
	db, _ := staleSeed(t)
	out, err := memTimeline(db, map[string]any{"anchor": 3.0, "depth_before": 2.0, "depth_after": 2.0})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "Deleted note <- **ANCHOR** (file removed since)") || !strings.Contains(out, "Changed note (file changed since)") {
		t.Errorf("timeline notes not marked:\n%s", out)
	}
	if strings.Contains(out, "Fine note (file") || strings.Contains(out, "Legacy note (file") {
		t.Errorf("fresh notes marked:\n%s", out)
	}
}
