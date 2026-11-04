package main

// memory usage ingest: one api_usage row per API response, taken from the assistant lines of
// Claude Code transcripts. Claude Code writes a response once per content block, so the same
// message.id and requestId repeat; the pair is the primary key and every write is an upsert
// that keeps the larger count, so re-reading a file (or the one-time backfill overlapping the
// live tailer) adds nothing twice.

import (
	"bufio"
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// memdUsage is one API response's token counts (see api_usage). C5m and C1h are nil when the
// transcript has no cache_creation object.
type memdUsage struct {
	MessageID, RequestID, SessionID, Project, Cwd, Model, AgentID, ServiceTier, Reset string
	At                                                                                time.Time
	Sidechain, Observer                                                               bool
	In, Out, CacheCreate, CacheRead                                                   int64
	C5m, C1h                                                                          *int64
}

// memdUsageBackfillVersion is the import_watermarks value for table api_usage once the
// history under the roots has been read. Raise it to force one more pass.
const memdUsageBackfillVersion = 1

// usageFromLine inspects one decoded transcript line. A /clear or compaction marker sets
// st.Reset, which the next main-thread response takes. An assistant line with a usage object
// and a message id gives a row; synthetic messages (no API call) and everything else give nil.
func usageFromLine(path string, d *obj, st *memdFileState) *memdUsage {
	switch d.get("type") {
	case "system":
		if d.getStr("subtype") == "compact_boundary" {
			st.Reset = "compact"
		}
		return nil
	case "user":
		if truthy(d.get("isCompactSummary")) {
			st.Reset = "compact"
		} else if s, ok := d.getObj("message").get("content").(string); ok && strings.Contains(s, "<command-name>/clear</command-name>") {
			st.Reset = "clear"
		}
		return nil
	case "assistant":
	default:
		return nil
	}
	msg := d.getObj("message")
	u := msg.getObj("usage")
	id, model := msg.getStr("id"), msg.getStr("model")
	if u == nil || id == "" || model == "<synthetic>" {
		return nil
	}
	row := &memdUsage{
		MessageID: id, RequestID: d.getStr("requestId"), SessionID: d.getStr("sessionId"),
		Cwd: d.getStr("cwd"), Model: model, AgentID: d.getStr("agentId"),
		ServiceTier: u.getStr("service_tier"),
		Sidechain:   truthy(d.get("isSidechain")) || strings.Contains(path, "/subagents/"),
		Observer:    st.Observer,
		In:          numField(u, "input_tokens"), Out: numField(u, "output_tokens"),
		CacheCreate: numField(u, "cache_creation_input_tokens"), CacheRead: numField(u, "cache_read_input_tokens"),
	}
	if row.SessionID == "" {
		name := filepath.Base(path)
		row.SessionID = strings.TrimSuffix(name, filepath.Ext(name))
	}
	row.Project = projectKey(row.Cwd)
	if ts := d.getStr("timestamp"); ts != "" {
		row.At, _ = time.Parse(time.RFC3339Nano, ts)
	}
	if row.At.IsZero() {
		row.At = time.Unix(0, 0).UTC()
	}
	if cc := u.getObj("cache_creation"); cc != nil {
		a, b := numField(cc, "ephemeral_5m_input_tokens"), numField(cc, "ephemeral_1h_input_tokens")
		row.C5m, row.C1h = &a, &b
	}
	if !row.Sidechain && st.Reset != "" {
		row.Reset, st.Reset = st.Reset, ""
	}
	return row
}

// numFieldMax bounds a token count (2^40, about 1.1e12); no response has more. A larger
// value is a corrupt line and counts as 0, so it cannot overflow int64 or poison the totals.
const numFieldMax = 1 << 40

// numField reads a non-negative integer field; anything else, or a value above
// numFieldMax, is 0.
func numField(o *obj, k string) int64 {
	n, ok := o.get(k).(json.Number)
	if !ok {
		return 0
	}
	if i, err := n.Int64(); err == nil {
		if i > 0 && i <= numFieldMax {
			return i
		}
		return 0
	}
	if f, err := n.Float64(); err == nil && f > 0 && f <= numFieldMax {
		return int64(f)
	}
	return 0
}

// mergeUsage collapses rows with the same key: counts keep the larger value, the time the
// earlier one, and a reset marker survives.
func mergeUsage(rows []memdUsage) []memdUsage {
	idx := map[string]int{}
	var out []memdUsage
	for _, r := range rows {
		k := r.MessageID + "\x00" + r.RequestID
		i, ok := idx[k]
		if !ok {
			idx[k] = len(out)
			out = append(out, r)
			continue
		}
		m := &out[i]
		m.In, m.Out = max(m.In, r.In), max(m.Out, r.Out)
		m.CacheCreate, m.CacheRead = max(m.CacheCreate, r.CacheCreate), max(m.CacheRead, r.CacheRead)
		m.C5m, m.C1h = maxPtr(m.C5m, r.C5m), maxPtr(m.C1h, r.C1h)
		if r.At.Before(m.At) {
			m.At = r.At
		}
		if m.Reset == "" {
			m.Reset = r.Reset
		}
	}
	return out
}

func maxPtr(a, b *int64) *int64 {
	switch {
	case a == nil:
		return b
	case b == nil || *a >= *b:
		return a
	}
	return b
}

// usageBatch is how many rows one INSERT carries: 18 parameters each, well under the limit,
// and a statement per row costs about 3 ms in DuckDB where a statement per 200 rows does not.
const usageBatch = 200

const usageUpsertTail = ` ON CONFLICT (message_id, request_id) DO UPDATE SET
  ts = least(api_usage.ts, excluded.ts),
  input_tokens = greatest(api_usage.input_tokens, excluded.input_tokens),
  output_tokens = greatest(api_usage.output_tokens, excluded.output_tokens),
  cache_creation_input_tokens = greatest(api_usage.cache_creation_input_tokens, excluded.cache_creation_input_tokens),
  cache_creation_5m_input_tokens = greatest(api_usage.cache_creation_5m_input_tokens, excluded.cache_creation_5m_input_tokens),
  cache_creation_1h_input_tokens = greatest(api_usage.cache_creation_1h_input_tokens, excluded.cache_creation_1h_input_tokens),
  cache_read_input_tokens = greatest(api_usage.cache_read_input_tokens, excluded.cache_read_input_tokens),
  reset_kind = CASE WHEN api_usage.reset_kind <> '' THEN api_usage.reset_kind ELSE excluded.reset_kind END`

const usageInsertHead = `INSERT INTO api_usage (message_id, request_id, session_id, project, cwd, ts, model, is_sidechain, is_observer,
  agent_id, input_tokens, output_tokens, cache_creation_input_tokens, cache_creation_5m_input_tokens,
  cache_creation_1h_input_tokens, cache_read_input_tokens, service_tier, reset_kind) VALUES `

const usageTuple = `(?, ?, ?, ?, ?, CAST(? AS TIMESTAMP), ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`

// writeUsage upserts rows in tx, usageBatch rows per statement. Each key appears once per
// call (mergeUsage), which a multi-row ON CONFLICT needs, and an existing row only ever
// grows, so the result does not depend on how often a line is read.
func writeUsage(tx *sql.Tx, rows []memdUsage) error {
	rows = mergeUsage(rows)
	for len(rows) > 0 {
		n := min(usageBatch, len(rows))
		var q strings.Builder
		q.WriteString(usageInsertHead)
		args := make([]any, 0, n*18)
		for i, r := range rows[:n] {
			if i > 0 {
				q.WriteByte(',')
			}
			q.WriteString(usageTuple)
			var c5, c1 any
			if r.C5m != nil {
				c5 = *r.C5m
			}
			if r.C1h != nil {
				c1 = *r.C1h
			}
			args = append(args, r.MessageID, r.RequestID, r.SessionID, r.Project, r.Cwd, memdTS(r.At), r.Model, r.Sidechain, r.Observer,
				r.AgentID, r.In, r.Out, r.CacheCreate, c5, c1, r.CacheRead, r.ServiceTier, r.Reset)
		}
		q.WriteString(usageUpsertTail)
		if _, err := tx.Exec(q.String(), args...); err != nil {
			return fmt.Errorf("insert usage: %w", err)
		}
		rows = rows[n:]
	}
	return nil
}

// parseUsageLines is the backfill's parser: only usage rows and the markers that tag them,
// with the same latches parseMemdLines applies.
func parseUsageLines(path string, lines [][]byte, st *memdFileState) []memdUsage {
	st.Observer = st.Observer || strings.Contains(path, "memory-observer-cwd")
	var rows []memdUsage
	for _, raw := range lines {
		if !bytes.Contains(raw, []byte(`"usage"`)) && !bytes.Contains(raw, []byte("compact")) &&
			!bytes.Contains(raw, []byte("<command-name>/clear")) {
			continue
		}
		v, err := decodeJSON(raw)
		if err != nil {
			continue
		}
		d, ok := v.(*obj)
		if !ok {
			continue
		}
		if strings.HasPrefix(d.getStr("entrypoint"), "sdk") {
			st.Observer = true
		}
		if u := usageFromLine(path, d, st); u != nil {
			rows = append(rows, *u)
		}
	}
	return rows
}

const usageBackfillChunk = 5000

// usageWatermarkKey is the import_watermarks row recording that root's usage history was read.
func usageWatermarkKey(root string) string { return "api_usage:" + filepath.Clean(root) }

// BackfillUsage reads every transcript under each root from the start and upserts its usage
// rows, once per root: import_watermarks keeps usageWatermarkKey(root) at
// memdUsageBackfillVersion afterwards, so a root added later (another account) is still read.
// The tailer starts old files at their end, so this is the only reader of history. A failing
// file leaves its root's watermark unset, so the next start tries again; rows are upserts
// either way.
func (t *memdTailer) BackfillUsage(ctx context.Context) error {
	var firstErr error
	for root := range t.roots {
		var done int64
		err := t.db.QueryRow(`SELECT last_id FROM import_watermarks WHERE table_name = ?`, usageWatermarkKey(root)).Scan(&done)
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return fmt.Errorf("usage watermark: %w", err)
		}
		if done >= memdUsageBackfillVersion {
			continue
		}
		var rootErr error
		filepath.WalkDir(root, func(path string, d fs.DirEntry, werr error) error {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			if werr != nil || d.IsDir() || !strings.HasSuffix(path, ".jsonl") {
				return nil
			}
			if err := t.backfillFile(path); err != nil && rootErr == nil {
				rootErr = fmt.Errorf("usage backfill %s: %w", path, err)
			}
			return nil
		})
		if err := ctx.Err(); err != nil {
			return err
		}
		if rootErr != nil {
			if firstErr == nil {
				firstErr = rootErr
			}
			continue
		}
		if err := withWrite(func() error {
			_, err := t.db.Exec(`INSERT INTO import_watermarks (table_name, last_id) VALUES (?, ?)
ON CONFLICT (table_name) DO UPDATE SET last_id = excluded.last_id`, usageWatermarkKey(root), memdUsageBackfillVersion)
			return err
		}); err != nil && firstErr == nil {
			firstErr = err
		}
	}
	return firstErr
}

func (t *memdTailer) backfillFile(path string) error {
	fh, err := os.Open(path)
	if err != nil {
		return nil // vanished
	}
	defer fh.Close()
	br := bufio.NewReaderSize(fh, 1<<20)
	var st memdFileState
	var lines [][]byte
	flush := func() error {
		rows := parseUsageLines(path, lines, &st)
		lines = lines[:0]
		if len(rows) == 0 {
			return nil
		}
		return withWrite(func() error {
			tx, err := t.db.Begin()
			if err != nil {
				return err
			}
			defer tx.Rollback()
			if err := writeUsage(tx, rows); err != nil {
				return err
			}
			return tx.Commit()
		})
	}
	for {
		line, err := br.ReadBytes('\n')
		if err == nil { // a last line without its newline is still being written; the tailer owns it
			lines = append(lines, bytes.TrimRight(line, "\r\n"))
			if len(lines) >= usageBackfillChunk {
				if err := flush(); err != nil {
					return err
				}
			}
			continue
		}
		if err != io.EOF {
			return err
		}
		return flush()
	}
}
