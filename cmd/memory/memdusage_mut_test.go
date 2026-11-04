package main

// Boundary and merge tests that pin the upsert (greatest) semantics, the attribution edges and the price tiers.

import (
	"database/sql"
	"net/http/httptest"
	"regexp"
	"strings"
	"testing"
	"time"
)

func mutRow(fn func(r *memdUsage)) memdUsage {
	r := memdUsage{MessageID: "m", RequestID: "r", SessionID: "s", Model: "claude-opus-5-5", At: time.Date(2026, 11, 3, 10, 0, 10, 0, time.UTC)}
	fn(&r)
	return r
}

type mutSnap struct {
	in, out, cc, cr int64
	c5, c1          sql.NullInt64
	ts, reset       string
}

func mutRead(t *testing.T, db *sql.DB) mutSnap {
	t.Helper()
	var s mutSnap
	err := db.QueryRow(`SELECT input_tokens, output_tokens, cache_creation_input_tokens, cache_read_input_tokens,
		cache_creation_5m_input_tokens, cache_creation_1h_input_tokens, strftime(ts, '%H:%M:%S'), reset_kind FROM api_usage`).
		Scan(&s.in, &s.out, &s.cc, &s.cr, &s.c5, &s.c1, &s.ts, &s.reset)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func mutWrite(t *testing.T, db *sql.DB, rows ...memdUsage) {
	t.Helper()
	tx, err := db.Begin()
	if err != nil {
		t.Fatal(err)
	}
	if err := writeUsage(tx, rows); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
}

// Streaming partials: the same key with different counts, in both orders, in one call and in
// separate transactions. Every count keeps the larger value; ts the earlier; reset survives.
func TestUsageUpsertKeepsLargestCounts(t *testing.T) {
	small := mutRow(func(r *memdUsage) {
		r.In, r.Out, r.CacheCreate, r.CacheRead = 1, 16, 100, 1000
		r.C5m, r.C1h = i64(60), i64(40)
		r.At = r.At.Add(5 * time.Second)
	})
	big := mutRow(func(r *memdUsage) {
		r.In, r.Out, r.CacheCreate, r.CacheRead = 2, 307, 200, 2000
		r.C5m, r.C1h = i64(120), i64(80)
		r.Reset = "clear"
	})
	for name, seq := range map[string][][]memdUsage{
		"small then big, two txs":  {{small}, {big}},
		"big then small, two txs":  {{big}, {small}},
		"small,big one call":       {{small, big}},
		"big,small one call":       {{big, small}},
		"stale small after final":  {{small}, {big}, {small}},
		"split-less late partial":  {{big}, {mutRow(func(r *memdUsage) { r.Out = 5 })}},
		"split-less first partial": {{mutRow(func(r *memdUsage) { r.Out = 5 })}, {big}},
	} {
		t.Run(name, func(t *testing.T) {
			db, _ := openTestMemd(t)
			for _, rows := range seq {
				mutWrite(t, db, rows...)
			}
			s := mutRead(t, db)
			if s.in != 2 || s.out != 307 || s.cc != 200 || s.cr != 2000 || !s.c5.Valid || s.c5.Int64 != 120 || !s.c1.Valid || s.c1.Int64 != 80 {
				t.Errorf("counts = %+v, want in 2 out 307 cc 200 cr 2000 c5 120 c1 80", s)
			}
			if s.reset != "clear" {
				t.Errorf("reset = %q, want clear", s.reset)
			}
			if !strings.Contains(name, "split-less") && s.ts != "10:00:10" && s.ts != "10:00:15" {
				t.Errorf("ts = %s", s.ts)
			}
			if strings.Contains(name, "small") && !strings.Contains(name, "split-less") && s.ts != "10:00:10" {
				t.Errorf("ts = %s, want the earlier 10:00:10", s.ts)
			}
		})
	}
	// ts: the earlier of two sightings wins whichever is written first.
	for _, first := range []string{"early first", "late first"} {
		db, _ := openTestMemd(t)
		e, l := mutRow(func(r *memdUsage) {}), mutRow(func(r *memdUsage) { r.At = r.At.Add(time.Hour) })
		if first == "early first" {
			mutWrite(t, db, e)
			mutWrite(t, db, l)
		} else {
			mutWrite(t, db, l)
			mutWrite(t, db, e)
		}
		if s := mutRead(t, db); s.ts != "10:00:10" {
			t.Errorf("%s: ts = %s", first, s.ts)
		}
	}
	// reset: once set it is kept; a later sighting without one does not clear it, a later one with
	// a different kind does not replace it.
	db, _ := openTestMemd(t)
	mutWrite(t, db, mutRow(func(r *memdUsage) { r.Reset = "compact" }))
	mutWrite(t, db, mutRow(func(r *memdUsage) {}))
	mutWrite(t, db, mutRow(func(r *memdUsage) { r.Reset = "clear" }))
	if s := mutRead(t, db); s.reset != "compact" {
		t.Errorf("reset = %q, want compact", s.reset)
	}
}

func TestUsageNullSplitStaysNull(t *testing.T) {
	var st memdFileState
	rows := parseUsageLines("/p/s.jsonl", [][]byte{[]byte(`{"type":"assistant","sessionId":"s","timestamp":"2026-11-03T10:00:00Z","message":{"id":"m","model":"claude-opus-5-5","usage":{"input_tokens":1,"cache_creation_input_tokens":5}}}`)}, &st)
	if len(rows) != 1 || rows[0].C5m != nil || rows[0].C1h != nil {
		t.Fatalf("rows = %+v, want one row with nil split", rows)
	}
	db, _ := openTestMemd(t)
	mutWrite(t, db, rows...)
	if s := mutRead(t, db); s.c5.Valid || s.c1.Valid {
		t.Errorf("split stored as %+v, want NULL", s)
	}
}

func TestUsageSidechainDoesNotTakeReset(t *testing.T) {
	var st memdFileState
	mk := func(id, side string) []byte {
		return []byte(`{"type":"assistant","sessionId":"s","isSidechain":` + side + `,"timestamp":"2026-11-03T10:00:00Z","message":{"id":"` + id + `","model":"claude-opus-5-5","usage":{"input_tokens":1}}}`)
	}
	lines := [][]byte{
		[]byte(`{"type":"user","message":{"content":"<command-name>/clear</command-name>"}}`),
		mk("side", "true"), mk("main", "false"), mk("main2", "false"),
	}
	rows := parseUsageLines("/p/s.jsonl", lines, &st)
	got := map[string]string{}
	for _, r := range rows {
		got[r.MessageID] = r.Reset
	}
	if got["side"] != "" || got["main"] != "clear" || got["main2"] != "" {
		t.Fatalf("resets = %v, want side '' main clear main2 ''", got)
	}
}

func TestCostAttributionBoundaries(t *testing.T) {
	op, so := "claude-opus-5-5", "claude-sonnet-5-5"
	// gap exactly 5m / exactly 1h is NOT idle; one second more is.
	at := func(sec int) costRow {
		r := crow(0, op, 30000, 30000, 0)
		r.At = cbase.Add(time.Duration(sec) * time.Second)
		return r
	}
	prev5 := crow(-100, op, 100, 100, 0)
	prev1 := crow(-100, op, 100, 0, 100)
	cases := []struct {
		name string
		prev costRow
		cur  costRow
		want string
	}{
		{"5m exactly", prev5, at(-100*60 + 300), causeOther},
		{"5m+1s", prev5, at(-100*60 + 301), causeIdle5m},
		{"1h exactly", prev1, at(-100*60 + 3600), causeOther},
		{"1h+1s", prev1, at(-100*60 + 3601), causeIdle1h},
		{"mixed 5m+1h write is a 5m entry", crow(-100, op, 100, 50, 50), at(-100*60 + 400), causeIdle5m},
		{"model switch and idle: idle wins", prev5, func() costRow { r := at(-100*60 + 400); r.Model = so; return r }(), causeIdle5m},
		{"model switch only", prev5, func() costRow { r := at(-100*60 + 30); r.Model = so; return r }(), causeModel},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := attributeCache([]costRow{c.prev, c.cur}, 20000)
			if len(got) != 1 || got[0].Cause != c.want {
				t.Fatalf("got %+v, want cause %s", got, c.want)
			}
		})
	}
	// "large" is >= min: a write of exactly minCache counts.
	if got := attributeCache([]costRow{crow(0, op, 100, 100, 0), crow(1, op, 20000, 20000, 0)}, 20000); len(got) != 1 {
		t.Errorf("write of exactly min_cache not reported: %+v", got)
	}
	if got := attributeCache([]costRow{crow(0, op, 100, 100, 0), crow(1, op, 19999, 19999, 0)}, 20000); len(got) != 0 {
		t.Errorf("write below min_cache reported: %+v", got)
	}
	// A read (no write) between two writes keeps the 1h entry's lifetime.
	rd := crow(30, op, 0, 0, 0)
	rows := []costRow{crow(0, op, 100, 0, 100), rd, crow(30+10, op, 30000, 30000, 0)}
	if got := attributeCache(rows, 20000); len(got) != 1 || got[0].Cause != causeOther {
		t.Errorf("read refresh: %+v", got)
	}
}

func TestCostHaikuTierBoundary(t *testing.T) {
	m := "claude-haiku-5-5"
	// prompt = in + create + read. Exactly 100000 is the short tier; 100001 the long tier; reads count.
	for _, c := range []struct {
		r    costRow
		long bool
	}{
		{costRow{Model: m, In: 10, Create: 50000, Read: 49990}, false},
		{costRow{Model: m, In: 11, Create: 50000, Read: 49990}, true},
		{costRow{Model: m, In: 1, Create: 0, Read: 100000}, true},
		{costRow{Model: m, In: 1, Create: 100000, Read: 0}, true},
		{costRow{Model: m, In: 100000, Create: 0, Read: 0}, false},
	} {
		p, _ := priceFor(m, c.r.In+c.r.Create+c.r.Read)
		if (p == haikuLongPrice) != c.long {
			t.Errorf("%+v: long=%v want %v", c.r, p == haikuLongPrice, c.long)
		}
		total, _, _, _ := turnCost(c.r)
		want := float64(c.r.In)*p.In/1e6 + float64(c.r.Create)*p.W5m/1e6 + float64(c.r.Read)*p.Read/1e6
		if !near(total, want) {
			t.Errorf("%+v total %v want %v", c.r, total, want)
		}
	}
}

func TestCostReportWindowAndDefaults(t *testing.T) {
	db, _ := openTestMemd(t)
	op := "claude-opus-5-5"
	costSeed(t, db, "old", "s1", -3*24*60, op, false, 1, 1, 100, 0, int64(100), int64(0))
	costSeed(t, db, "new", "s1", 0, op, false, 1, 1, 50000, 0, int64(50000), int64(0))
	out, err := buildCostReport(db, costOpts{Days: 1, MinCache: 20000, Top: 10, Now: cbase.Add(time.Hour)})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "total: 1 turns,") || strings.Contains(out, "2026-10-31") || !strings.Contains(out, "excluded 0 subagent") {
		t.Fatalf("window report counts out-of-window turns:\n%s", out)
	}
}

func TestCostHTTPDefaults(t *testing.T) {
	db, _ := openTestMemd(t)
	costSeed(t, db, "a", "s1", 0, "claude-opus-5-5", false, 1, 1, 1000, 0, int64(1000), int64(0))
	h := memdHandler(db)
	get := func(q string) string {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest("GET", "/cost"+q, nil))
		return rec.Body.String()
	}
	// default min_cache 20000: a 1000-token first write is not "large"; min_cache=0 makes it so.
	if out := get("?days=0"); !regexp.MustCompile(`all large\s+0 turns`).MatchString(out) {
		t.Errorf("default min_cache:\n%s", out)
	}
	if out := get("?days=0&min_cache=0"); regexp.MustCompile(`all large\s+0 turns`).MatchString(out) {
		t.Errorf("min_cache=0:\n%s", out)
	}
}
