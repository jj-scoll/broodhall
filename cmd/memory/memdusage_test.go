package main

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// usageLine is an assistant transcript line shaped like a real one: one per content block, so
// the same message.id and requestId repeat. split adds the cache_creation object.
type usageLine struct {
	sess, mid, rid, model, ts, agent string
	side                             bool
	in, out, create, read            int
	split                            *[2]int // 5m, 1h
	tier                             string
}

func (u usageLine) String() string {
	cc := ""
	if u.split != nil {
		cc = fmt.Sprintf(`,"cache_creation":{"ephemeral_1h_input_tokens":%d,"ephemeral_5m_input_tokens":%d}`, u.split[1], u.split[0])
	}
	tier := ""
	if u.tier != "" {
		tier = fmt.Sprintf(`,"service_tier":%q`, u.tier)
	}
	rid := ""
	if u.rid != "" {
		rid = fmt.Sprintf(`"requestId":%q,`, u.rid)
	}
	agent := ""
	if u.agent != "" {
		agent = fmt.Sprintf(`"agentId":%q,`, u.agent)
	}
	return fmt.Sprintf(`{"type":"assistant","sessionId":%q,"cwd":"/work/demo","entrypoint":"cli","isSidechain":%v,%s%s"timestamp":%q,"message":{"id":%q,"role":"assistant","model":%q,"stop_reason":"tool_use","content":[{"type":"text","text":"x"}],"usage":{"input_tokens":%d,"cache_creation_input_tokens":%d,"cache_read_input_tokens":%d,"output_tokens":%d%s%s}}}`+"\n",
		u.sess, u.side, rid, agent, u.ts, u.mid, u.model, u.in, u.create, u.read, u.out, tier, cc)
}

func usageCount(t *testing.T, db *sql.DB) int {
	t.Helper()
	var n int
	if err := db.QueryRow(`SELECT count(*) FROM api_usage`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

func writeTranscript(t *testing.T, dir, name, body string) string {
	t.Helper()
	p := filepath.Join(dir, name)
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestUsageDedupesContentBlocks(t *testing.T) {
	e := newTailEnv(t, "")
	a := usageLine{sess: "s1", mid: "msg_A", rid: "req_A", model: "claude-opus-5-5", ts: tailTS(1, 0, 0), in: 2, out: 433, create: 467, read: 44731, split: &[2]int{0, 467}, tier: "standard"}
	b := usageLine{sess: "s1", mid: "msg_B", rid: "req_B", model: "claude-opus-5-5", ts: tailTS(1, 0, 5), in: 3, out: 7, create: 10, read: 20}
	a2, a3 := a, a
	a2.ts, a3.ts = tailTS(1, 0, 1), tailTS(1, 0, 2)
	writeTranscript(t, e.dir, "s1.jsonl", a.String()+a2.String()+a3.String()+b.String())
	if _, err := e.tailer().Scan(); err != nil {
		t.Fatal(err)
	}
	if n := usageCount(t, e.db); n != 2 {
		t.Fatalf("api_usage rows = %d, want 2 (three lines of msg_A are one response)", n)
	}
	var in, out, cc, c5, c1, cr int64
	var tier string
	var ts string
	if err := e.db.QueryRow(`SELECT input_tokens, output_tokens, cache_creation_input_tokens, cache_creation_5m_input_tokens,
		cache_creation_1h_input_tokens, cache_read_input_tokens, service_tier, strftime(ts, '%H:%M:%S') FROM api_usage WHERE message_id='msg_A'`).
		Scan(&in, &out, &cc, &c5, &c1, &cr, &tier, &ts); err != nil {
		t.Fatal(err)
	}
	if in != 2 || out != 433 || cc != 467 || c5 != 0 || c1 != 467 || cr != 44731 || tier != "standard" || ts != "01:00:00" {
		t.Fatalf("msg_A row = in %d out %d cc %d c5 %d c1 %d cr %d tier %q ts %s", in, out, cc, c5, c1, cr, tier, ts)
	}
	// No cache_creation object: the split stays NULL (unknown), not 0.
	var n5 sql.NullInt64
	if err := e.db.QueryRow(`SELECT cache_creation_5m_input_tokens FROM api_usage WHERE message_id='msg_B'`).Scan(&n5); err != nil {
		t.Fatal(err)
	}
	if n5.Valid {
		t.Fatalf("msg_B 5m split = %d, want NULL", n5.Int64)
	}
}

func TestUsageSameMessageDifferentRequestIsTwoRows(t *testing.T) {
	e := newTailEnv(t, "")
	a := usageLine{sess: "s1", mid: "msg_A", rid: "req_1", model: "m", ts: tailTS(1, 0, 0), out: 1}
	b := a
	b.rid = "req_2"
	writeTranscript(t, e.dir, "s1.jsonl", a.String()+b.String())
	e.tailer().Scan()
	if n := usageCount(t, e.db); n != 2 {
		t.Fatalf("rows = %d, want 2: the key is message.id plus requestId", n)
	}
}

func TestUsageNoRequestIDIsOneRow(t *testing.T) {
	e := newTailEnv(t, "")
	a := usageLine{sess: "s1", mid: "msg_A", model: "claude-opus-5-5", ts: tailTS(1, 0, 0), out: 5}
	a2 := a
	a2.ts = tailTS(1, 0, 1)
	writeTranscript(t, e.dir, "s1.jsonl", a.String()+a2.String())
	e.tailer().Scan()
	if n := usageCount(t, e.db); n != 1 {
		t.Fatalf("rows = %d, want 1 (classifier calls carry no requestId)", n)
	}
}

func TestUsageSkipsSyntheticAndUsageless(t *testing.T) {
	e := newTailEnv(t, "")
	syn := usageLine{sess: "s1", mid: "msg_S", rid: "r", model: "<synthetic>", ts: tailTS(1, 0, 0)}
	noUsage := `{"type":"assistant","sessionId":"s1","timestamp":"` + tailTS(1, 0, 1) + `","message":{"id":"msg_N","model":"claude-opus-5-5","content":[]}}` + "\n"
	writeTranscript(t, e.dir, "s1.jsonl", syn.String()+noUsage)
	e.tailer().Scan()
	if n := usageCount(t, e.db); n != 0 {
		t.Fatalf("rows = %d, want 0", n)
	}
}

func TestUsageKeepsEventsBeforeDaemonStart(t *testing.T) {
	e := newTailEnv(t, "")
	old := usageLine{sess: "s1", mid: "msg_O", rid: "r", model: "claude-opus-5-5", ts: "2026-10-20T01:00:00.000Z", out: 1}
	writeTranscript(t, e.dir, "s1.jsonl", old.String())
	e.tailer().Scan()
	if n := usageCount(t, e.db); n != 1 {
		t.Fatalf("rows = %d, want 1: prompts older than startedAt are dropped, usage is not", n)
	}
}

func TestUsageSidechainAndAgent(t *testing.T) {
	e := newTailEnv(t, "")
	main := usageLine{sess: "s1", mid: "msg_M", rid: "r1", model: "m", ts: tailTS(1, 0, 0), out: 1}
	sub := usageLine{sess: "s1", mid: "msg_X", rid: "r2", model: "m", ts: tailTS(1, 0, 1), out: 1, side: true, agent: "a123"}
	// A subagent file is a sidechain by path even if a line forgets the flag.
	pathOnly := usageLine{sess: "s1", mid: "msg_Y", rid: "r3", model: "m", ts: tailTS(1, 0, 2), out: 1, agent: "a123"}
	writeTranscript(t, e.dir, "s1.jsonl", main.String())
	writeTranscript(t, e.dir, "s1/subagents/agent-a123.jsonl", sub.String()+pathOnly.String())
	if _, err := e.tailer().Scan(); err != nil {
		t.Fatal(err)
	}
	rows, err := e.db.Query(`SELECT message_id, is_sidechain, agent_id FROM api_usage ORDER BY message_id`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var got []string
	for rows.Next() {
		var id, agent string
		var side bool
		rows.Scan(&id, &side, &agent)
		got = append(got, fmt.Sprintf("%s/%v/%s", id, side, agent))
	}
	want := "msg_M/false/ msg_X/true/a123 msg_Y/true/a123"
	if strings.Join(got, " ") != want {
		t.Fatalf("rows = %v, want %s", got, want)
	}
}

func TestUsageResetKinds(t *testing.T) {
	e := newTailEnv(t, "")
	clear := `{"type":"user","sessionId":"s1","timestamp":"` + tailTS(1, 0, 1) + `","message":{"role":"user","content":"<command-name>/clear</command-name>\n<command-message>clear</command-message>"}}` + "\n"
	compact := `{"type":"user","sessionId":"s1","isCompactSummary":true,"timestamp":"` + tailTS(1, 0, 3) + `","message":{"role":"user","content":"This session is being continued from a previous conversation"}}` + "\n"
	boundary := `{"type":"system","subtype":"compact_boundary","sessionId":"s1","timestamp":"` + tailTS(1, 0, 5) + `"}` + "\n"
	u := func(id, s int) string {
		return usageLine{sess: "s1", mid: fmt.Sprintf("m%d", id), rid: "r", model: "m", ts: tailTS(1, 0, s), out: 1}.String()
	}
	writeTranscript(t, e.dir, "s1.jsonl", u(0, 0)+clear+u(1, 2)+compact+u(2, 4)+u(3, 4)+boundary+u(4, 6))
	e.tailer().Scan()
	rows, _ := e.db.Query(`SELECT message_id, reset_kind FROM api_usage ORDER BY message_id`)
	defer rows.Close()
	var got []string
	for rows.Next() {
		var id, k string
		rows.Scan(&id, &k)
		got = append(got, id+"="+k)
	}
	if strings.Join(got, " ") != "m0= m1=clear m2=compact m3= m4=compact" {
		t.Fatalf("reset kinds = %v", got)
	}
}

func TestUsageReingestIsIdempotent(t *testing.T) {
	e := newTailEnv(t, "")
	a := usageLine{sess: "s1", mid: "msg_A", rid: "req_A", model: "m", ts: tailTS(1, 0, 0), in: 2, out: 9, create: 5, read: 7}
	a2 := a
	a2.ts = tailTS(1, 0, 1)
	writeTranscript(t, e.dir, "s1.jsonl", a.String()+a2.String())
	e.tailer().Scan()
	sum := func() string {
		var s string
		if err := e.db.QueryRow(`SELECT count(*)||'/'||sum(input_tokens+output_tokens+cache_creation_input_tokens+cache_read_input_tokens) FROM api_usage`).Scan(&s); err != nil {
			t.Fatal(err)
		}
		return s
	}
	first := sum()
	if first != "1/23" {
		t.Fatalf("first = %s, want 1/23", first)
	}
	// A new tailer with the offsets cleared re-reads the file from byte 0.
	if _, err := e.db.Exec(`DELETE FROM transcript_offsets`); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 2; i++ {
		e.tailer().Scan()
		if got := sum(); got != first {
			t.Fatalf("after re-ingest %d: %s, want %s", i+1, got, first)
		}
	}
}

func TestUsageBackfillReadsFilesTheTailerSkipped(t *testing.T) {
	e := newTailEnv(t, "")
	a := usageLine{sess: "s1", mid: "msg_A", rid: "req_A", model: "m", ts: "2026-09-01T01:00:00.000Z", out: 4, create: 6}
	a2 := a
	a2.ts = "2026-09-01T01:00:01.000Z"
	b := usageLine{sess: "s1", mid: "msg_B", rid: "req_B", model: "m", ts: "2026-09-01T01:01:00.000Z", out: 1}
	sub := usageLine{sess: "s1", mid: "msg_C", rid: "req_C", model: "m", ts: "2026-09-01T01:02:00.000Z", out: 1, side: true, agent: "ag"}
	p1 := writeTranscript(t, e.dir, "s1.jsonl", a.String()+a2.String()+b.String())
	p2 := writeTranscript(t, e.dir, "s1/subagents/agent-ag.jsonl", sub.String())
	old := time.Date(2026, 9, 1, 2, 0, 0, 0, time.UTC)
	os.Chtimes(p1, old, old)
	os.Chtimes(p2, old, old)
	tl := e.tailer()
	if _, err := tl.Scan(); err != nil {
		t.Fatal(err)
	}
	if n := usageCount(t, e.db); n != 0 {
		t.Fatalf("the tailer read old files: %d rows", n)
	}
	for i := 0; i < 3; i++ {
		if err := tl.BackfillUsage(context.Background()); err != nil {
			t.Fatal(err)
		}
		if n := usageCount(t, e.db); n != 3 {
			t.Fatalf("backfill run %d: rows = %d, want 3", i+1, n)
		}
	}
	var wm int64
	if err := e.db.QueryRow(`SELECT last_id FROM import_watermarks WHERE table_name = ?`, usageWatermarkKey(e.root)).Scan(&wm); err != nil || wm != memdUsageBackfillVersion {
		t.Fatalf("watermark = %d (%v), want %d", wm, err, memdUsageBackfillVersion)
	}
	// With the watermark set the pass does not read files again: a new old file is left alone.
	c := usageLine{sess: "s2", mid: "msg_D", rid: "req_D", model: "m", ts: "2026-09-02T01:00:00.000Z", out: 1}
	p3 := writeTranscript(t, e.dir, "s2.jsonl", c.String())
	os.Chtimes(p3, old, old)
	tl.BackfillUsage(context.Background())
	if n := usageCount(t, e.db); n != 3 {
		t.Fatalf("backfill ran again after the watermark: rows = %d", n)
	}
	// Dropping the watermark re-runs it, still without double counting.
	e.db.Exec(`DELETE FROM import_watermarks WHERE table_name = ?`, usageWatermarkKey(e.root))
	tl.BackfillUsage(context.Background())
	var out int64
	e.db.QueryRow(`SELECT sum(output_tokens) FROM api_usage`).Scan(&out)
	if n := usageCount(t, e.db); n != 4 || out != 4+1+1+1 {
		t.Fatalf("rerun: rows = %d out = %d, want 4 and 7", n, out)
	}
}

// The usage history of each root is read once per root: a root added later (another account's
// config dir) is backfilled even though the roots read before, and the pre-root watermark,
// are already done; the roots already read are not read again.
func TestUsageBackfillPerRoot(t *testing.T) {
	e := newTailEnv(t, "")
	old := time.Date(2026, 9, 1, 2, 0, 0, 0, time.UTC)
	a := usageLine{sess: "s1", mid: "msg_A", rid: "req_A", model: "m", ts: "2026-09-01T01:00:00.000Z", out: 1}
	os.Chtimes(writeTranscript(t, e.dir, "s1.jsonl", a.String()), old, old)
	if err := e.tailer().BackfillUsage(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, err := e.db.Exec(`INSERT INTO import_watermarks VALUES ('api_usage', 1) ON CONFLICT (table_name) DO NOTHING`); err != nil {
		t.Fatal(err)
	}

	other := t.TempDir()
	b := usageLine{sess: "s2", mid: "msg_B", rid: "req_B", model: "m", ts: "2026-09-01T01:00:00.000Z", out: 1}
	os.Chtimes(writeTranscript(t, filepath.Join(other, "projects", "-work-demo"), "s2.jsonl", b.String()), old, old)
	c := usageLine{sess: "s3", mid: "msg_C", rid: "req_C", model: "m", ts: "2026-09-01T01:00:00.000Z", out: 1}
	os.Chtimes(writeTranscript(t, e.dir, "s3.jsonl", c.String()), old, old) // root already read

	tl := newMemdTailer(e.db, map[string]string{e.root: "a", other: "b"}, tailStart)
	if err := tl.BackfillUsage(context.Background()); err != nil {
		t.Fatal(err)
	}
	for mid, want := range map[string]int{"msg_A": 1, "msg_B": 1, "msg_C": 0} {
		if got := tailCount(t, e.db, "SELECT count(*) FROM api_usage WHERE message_id = ?", mid); got != want {
			t.Errorf("%s rows = %d, want %d", mid, got, want)
		}
	}
}

func TestUsageBackfillAndTailerOverlap(t *testing.T) {
	e := newTailEnv(t, "")
	a := usageLine{sess: "s1", mid: "msg_A", rid: "req_A", model: "m", ts: tailTS(1, 0, 0), out: 4}
	writeTranscript(t, e.dir, "s1.jsonl", a.String())
	tl := e.tailer()
	tl.Scan()
	tl.BackfillUsage(context.Background())
	var out int64
	e.db.QueryRow(`SELECT sum(output_tokens) FROM api_usage`).Scan(&out)
	if n := usageCount(t, e.db); n != 1 || out != 4 {
		t.Fatalf("rows = %d out = %d, want 1 and 4", n, out)
	}
}

func TestUsageMigratesDBWithoutTable(t *testing.T) {
	db, _ := openTestMemd(t)
	if _, err := db.Exec(`INSERT INTO import_watermarks VALUES ('observations', 7)`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`DROP TABLE api_usage`); err != nil { // the shape of a DB made before this change
		t.Fatal(err)
	}
	if err := memdMigrate(db); err != nil {
		t.Fatal(err)
	}
	if n := usageCount(t, db); n != 0 {
		t.Fatalf("rows = %d", n)
	}
	var wm int64
	if err := db.QueryRow(`SELECT last_id FROM import_watermarks WHERE table_name='observations'`).Scan(&wm); err != nil || wm != 7 {
		t.Fatalf("existing data lost: %d %v", wm, err)
	}
}

func TestUsageResetSurvivesRestart(t *testing.T) {
	e := newTailEnv(t, "")
	u := func(id, s int) string {
		return usageLine{sess: "s1", mid: fmt.Sprintf("m%d", id), rid: "r", model: "m", ts: tailTS(1, 0, s), out: 1}.String()
	}
	clear := `{"type":"user","sessionId":"s1","timestamp":"` + tailTS(1, 0, 1) + `","message":{"role":"user","content":"<command-name>/clear</command-name>"}}` + "\n"
	p := writeTranscript(t, e.dir, "s1.jsonl", u(0, 0)+clear)
	if _, err := e.tailer().Scan(); err != nil {
		t.Fatal(err)
	}
	// The daemon restarts between the marker and the next response.
	f, err := os.OpenFile(p, os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	f.WriteString(u(1, 2))
	f.Close()
	if _, err := e.tailer().Scan(); err != nil {
		t.Fatal(err)
	}
	var k string
	if err := e.db.QueryRow(`SELECT reset_kind FROM api_usage WHERE message_id = 'm1'`).Scan(&k); err != nil || k != "clear" {
		t.Fatalf("reset_kind after restart = %q (%v), want clear", k, err)
	}
}

func TestUsageMigratesTranscriptOffsetsWithoutResetColumn(t *testing.T) {
	db, _ := openTestMemd(t)
	if _, err := db.Exec(`INSERT INTO transcript_offsets (path, byte_offset) VALUES ('/p.jsonl', 10)`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`ALTER TABLE transcript_offsets DROP COLUMN reset_kind`); err != nil { // the shape origin/main made
		t.Fatal(err)
	}
	for i := 0; i < 2; i++ {
		if err := memdMigrate(db); err != nil {
			t.Fatal(err)
		}
	}
	var off int64
	var k string
	if err := db.QueryRow(`SELECT byte_offset, coalesce(reset_kind, '') FROM transcript_offsets`).Scan(&off, &k); err != nil || off != 10 || k != "" {
		t.Fatalf("row = %d %q (%v)", off, k, err)
	}
}

func TestNumFieldOutOfRange(t *testing.T) {
	for in, want := range map[string]int64{
		`{"n":1e30}`:                 0,
		`{"n":99999999999999999999}`: 0,
		`{"n":9223372036854775807}`:  0,
		`{"n":1099511627776}`:        1099511627776,
		`{"n":1099511627777}`:        0,
		`{"n":-5}`:                   0,
		`{"n":2.7}`:                  2,
		`{"n":"12"}`:                 0,
		`{"n":9.3e18}`:               0, // just above int64
	} {
		v, err := decodeJSON([]byte(in))
		if err != nil {
			t.Fatal(err)
		}
		if got := numField(v.(*obj), "n"); got != want {
			t.Errorf("numField(%s) = %d, want %d", in, got, want)
		}
	}
}

// usageSpeedFixture writes a transcript of n distinct responses, each repeated three times
// (content blocks), and returns the tailer that has not read it yet.
func usageSpeedFixture(t *testing.T, n int) *memdTailer {
	t.Helper()
	e := newTailEnv(t, "")
	var b strings.Builder
	for i := 0; i < n; i++ {
		for j := 0; j < 3; j++ {
			ts := time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC).Add(time.Duration(i)*time.Second + time.Duration(j)*time.Millisecond)
			b.WriteString(usageLine{sess: "s1", mid: fmt.Sprintf("msg_%05d", i), rid: fmt.Sprintf("req_%05d", i), model: "claude-opus-5-5",
				ts: ts.Format("2006-01-02T15:04:05.000Z"), in: 2, out: 100 + j, create: 500, read: 40000, split: &[2]int{0, 500}}.String())
		}
	}
	p := writeTranscript(t, e.dir, "s1.jsonl", b.String())
	old := time.Date(2026, 9, 1, 2, 0, 0, 0, time.UTC)
	os.Chtimes(p, old, old)
	return e.tailer()
}

func TestUsageBackfillSpeed(t *testing.T) {
	const n = 3400
	tl := usageSpeedFixture(t, n)
	t0 := time.Now()
	if err := tl.BackfillUsage(context.Background()); err != nil {
		t.Fatal(err)
	}
	d := time.Since(t0)
	t.Logf("backfill of %d keys (%d lines): %v", n, 3*n, d)
	if got := usageCount(t, tl.db); got != n {
		t.Fatalf("rows = %d, want %d", got, n)
	}
	var out int64
	tl.db.QueryRow(`SELECT sum(output_tokens) FROM api_usage`).Scan(&out)
	if out != int64(n)*102 {
		t.Fatalf("sum(output) = %d, want %d (the largest of three partials per key)", out, int64(n)*102)
	}
	// Dropping the watermark re-reads everything: every key conflicts, across batch edges too.
	tl.db.Exec(`DELETE FROM import_watermarks WHERE table_name LIKE 'api_usage%'`)
	if err := tl.BackfillUsage(context.Background()); err != nil {
		t.Fatal(err)
	}
	var out2 int64
	tl.db.QueryRow(`SELECT sum(output_tokens) FROM api_usage`).Scan(&out2)
	if usageCount(t, tl.db) != n || out2 != out {
		t.Fatalf("second pass changed totals: rows %d out %d, want %d and %d", usageCount(t, tl.db), out2, n, out)
	}
	if d > 20*time.Second {
		t.Fatalf("backfill took %v; want well under 20s (was about 57s per 3,400 keys with row-at-a-time upserts)", d)
	}
}
