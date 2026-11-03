package main

// memory observer workers: drain the queue through observe(), one observation per tool use.
// An unknown result (signed out, outage, CLI failure) never loses a row: it waits in state
// 'unknown' with its retry time in lease_until (R26) and is swept back to 'queued' when due.

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"math/rand"
	"strings"
	"sync"
	"time"
)

const (
	// observerWorkers is the default worker count.
	observerWorkers = 2
	// observerMaxAttempts caps rows whose unknown class is cli_error or
	// missing_structured_output (R25); auth and transport are uncapped.
	observerMaxAttempts = 5
	// observerLease outlives two observer calls (observe retries once on a validation failure).
	observerLease = 2*observerTimeout + 60*time.Second
)

// Timing knobs; vars so tests run in milliseconds.
var (
	observerBackoffBase = 30 * time.Second
	observerBackoffMax  = 15 * time.Minute
	observerPoll        = time.Second
	memdNow             = time.Now
	workerLogf          = log.Printf
)

// memdWriteMu serialises every worker DB write (R31): DuckDB fails concurrent writers with
// conflicts whatever their rows. The model call runs outside it. Other memory writers (the
// tailer, Task 21) must take it too. It also guards observerInflight.
var memdWriteMu sync.Mutex

// observerInflight holds dedupe keys whose observer call is running (under memdWriteMu).
var observerInflight = map[string]bool{}

// workerAfterItem is a test hook called after each processed item.
var workerAfterItem func()

const writeRetries = 12

// withWrite runs fn holding memdWriteMu. A transaction conflict (a backstop: the mutex
// should prevent them within this process) is retried with growing, jittered sleeps taken
// outside the lock.
func withWrite(fn func() error) error {
	var err error
	for try := 0; try < writeRetries; try++ {
		memdWriteMu.Lock()
		err = fn()
		memdWriteMu.Unlock()
		if err == nil || !isTxConflict(err) {
			return err
		}
		base := time.Duration(try+1) * 10 * time.Millisecond
		time.Sleep(base + time.Duration(rand.Int63n(int64(base))))
	}
	return fmt.Errorf("write still conflicting after %d tries: %w", writeRetries, err)
}

// observerBackoff is the retry delay after the given number of attempts: base doubling per
// attempt, capped.
func observerBackoff(attempts int) time.Duration {
	d := observerBackoffBase
	for i := 1; i < attempts && d < observerBackoffMax; i++ {
		d *= 2
	}
	if d > observerBackoffMax {
		d = observerBackoffMax
	}
	return d
}

// reasonClass is the R24 class of an unknown reason ("" for none).
func reasonClass(reason string) string {
	if i := strings.IndexByte(reason, ':'); i >= 0 {
		return reason[:i]
	}
	return reason
}

// runObserverWorkers runs n workers (default 2 when n <= 0) until ctx is cancelled and all
// of them have returned.
func runObserverWorkers(ctx context.Context, db *sql.DB, n int) {
	if n <= 0 {
		n = observerWorkers
	}
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			workerLoop(ctx, db)
		}()
	}
	wg.Wait()
}

func workerLoop(ctx context.Context, db *sql.DB) {
	for ctx.Err() == nil {
		if err := withWrite(func() error { return sweepQueue(db, memdNow()) }); err != nil {
			workerLogf("memory worker: sweep: %v", err)
		}
		var it *queueItem
		err := withWrite(func() (e error) { it, e = claimNext(db, memdNow(), observerLease); return })
		if err != nil {
			workerLogf("memory worker: claim: %v", err)
		}
		if it == nil {
			select {
			case <-ctx.Done():
			case <-time.After(observerPoll):
			}
			continue
		}
		if err := processItem(ctx, db, it); err != nil {
			workerLogf("memory worker: item %d: %v", it.ID, err)
		}
		if workerAfterItem != nil {
			workerAfterItem()
		}
	}
}

// sweepQueue returns expired claims and due unknown rows to 'queued'. Several workers sweep
// at once; a DuckDB conflict means another worker already moved the row.
func sweepQueue(db *sql.DB, now time.Time) error {
	if _, err := reclaimExpired(db, now); err != nil && !isTxConflict(err) {
		return err
	}
	_, err := db.Exec(`UPDATE queue SET state = 'queued', lease_until = NULL
		WHERE state = 'unknown' AND lease_until <= CAST(? AS TIMESTAMP)`, memdTS(now))
	if err != nil && !isTxConflict(err) {
		return fmt.Errorf("queue sweep unknown: %w", err)
	}
	return nil
}

type execer interface {
	Exec(query string, args ...any) (sql.Result, error)
}

// settle is the guarded (R15) outcome update for a claimed row: state, reason, optional
// retry-at in lease_until, and usage accumulated onto the row. ok=false is a lost lease.
func settle(ex execer, it *queueItem, state, reason string, retryAt *time.Time, u observerUsage) (bool, error) {
	var lease any
	if retryAt != nil {
		lease = memdTS(*retryAt)
	}
	res, err := ex.Exec(`UPDATE queue SET state = ?, reason = ?, lease_until = CAST(? AS TIMESTAMP),
		input_tokens = input_tokens + ?, output_tokens = output_tokens + ?,
		cache_read_tokens = cache_read_tokens + ?, cache_creation_tokens = cache_creation_tokens + ?,
		cost_usd = cost_usd + ?, updated_at = current_timestamp
		WHERE id = ? AND state = 'claimed' AND attempts = ?`,
		state, reason, lease, u.InputTokens, u.OutputTokens, u.CacheReadTokens, u.CacheCreationTokens, u.CostUSD,
		it.ID, it.Attempts)
	if err != nil {
		return false, fmt.Errorf("queue settle %d: %w", it.ID, err)
	}
	return rowsAffected(res) == 1, nil
}

// handBack returns an item to the retry list without a model result (R31 backstop), so a
// failed write never strands a row for a whole lease.
func handBack(db *sql.DB, it *queueItem, reason string) {
	retryAt := memdNow().Add(observerBackoff(it.Attempts))
	err := withWrite(func() error {
		_, e := settle(db, it, "unknown", reason, &retryAt, observerUsage{})
		return e
	})
	if err != nil {
		workerLogf("memory worker: hand back item %d: %v", it.ID, err)
	}
}

// processItem runs one claimed item through the observer and records the outcome. Health is
// recorded only for a result that was actually stored: a discarded (lost-lease) result
// writes nothing.
func processItem(ctx context.Context, db *sql.DB, it *queueItem) error {
	payload, _ := observerPayload(it)
	key := dedupeKey(observerPromptVersion, observerModel, payload)

	// Under the lock: claim the key (R15 guard), re-check observations and the in-flight set.
	var lost, dup, busy bool
	err := withWrite(func() error {
		lost, dup, busy = false, false, false
		res, err := db.Exec(`UPDATE queue SET dedupe_key = ? WHERE id = ? AND state = 'claimed' AND attempts = ?`, key, it.ID, it.Attempts)
		if err != nil {
			return fmt.Errorf("store dedupe key: %w", err)
		}
		if rowsAffected(res) != 1 {
			lost = true
			return nil
		}
		if err := db.QueryRow(`SELECT EXISTS (SELECT 1 FROM observations WHERE dedupe_key = ?)`, key).Scan(&dup); err != nil {
			return fmt.Errorf("dedupe check: %w", err)
		}
		if dup {
			_, err := finish(db, it.ID, it.Attempts, "done", "dedupe")
			return err
		}
		if observerInflight[key] {
			busy = true
			retryAt := memdNow().Add(observerPoll)
			_, err := settle(db, it, "unknown", "inflight", &retryAt, observerUsage{})
			return err
		}
		observerInflight[key] = true
		return nil
	})
	if err != nil {
		handBack(db, it, reasonTransport)
		return err
	}
	if lost || dup || busy {
		return nil
	}
	defer func() {
		memdWriteMu.Lock()
		delete(observerInflight, key)
		memdWriteMu.Unlock()
	}()

	obs, drops, outcome, reason, usage := observe(ctx, it)
	if ctx.Err() != nil {
		// Shutdown, not a result: hand the row back without spending an attempt.
		return withWrite(func() error {
			_, err := db.Exec(`UPDATE queue SET state = 'queued', lease_until = NULL, attempts = attempts - 1,
				input_tokens = input_tokens + ?, output_tokens = output_tokens + ?,
				cache_read_tokens = cache_read_tokens + ?, cache_creation_tokens = cache_creation_tokens + ?,
				cost_usd = cost_usd + ?
				WHERE id = ? AND state = 'claimed' AND attempts = ?`,
				usage.InputTokens, usage.OutputTokens, usage.CacheReadTokens, usage.CacheCreationTokens, usage.CostUSD,
				it.ID, it.Attempts)
			return err
		})
	}

	class := reasonClass(reason)
	err = withWrite(func() error {
		ok, err := recordOutcome(db, it, key, obs, drops, outcome, reason, class, usage)
		if err != nil || !ok {
			return err
		}
		// Not retried: a rerun would see the settled row as a lost lease.
		if herr := recordHealth(db, outcome, class, memdNow()); herr != nil {
			workerLogf("memory worker: health: %v", herr)
		}
		return nil
	})
	if err != nil {
		handBack(db, it, reasonTransport)
	}
	return err
}

// recordOutcome stores the result of one observe call (caller holds memdWriteMu). ok=false
// means the lease was lost and nothing was written.
func recordOutcome(db *sql.DB, it *queueItem, key string, obs *observation, drops []string, outcome, reason, class string, usage observerUsage) (bool, error) {
	switch outcome {
	case "done":
		oid, ok, err := storeObservationOnce(db, it, key, obs, usage)
		if err != nil || !ok {
			return ok, err
		}
		// Index last: the loss receipt below is written first, so a reader that sees the
		// settled item also sees its receipts. Still under the caller's memdWriteMu.
		defer memdIndexObservation(db, oid)
		if len(drops) > 0 {
			var facts, files int
			for _, d := range drops {
				if strings.HasPrefix(d, "fact:") {
					facts++
				} else {
					files++
				}
			}
			if err := recordLoss(db, "anchor_drop", fmt.Sprintf("facts=%d files=%d", facts, files), len(drops)); err != nil {
				return true, err
			}
		}
		return true, nil
	case "rejected":
		ok, err := settle(db, it, "rejected", reason, nil, usage)
		if err != nil || !ok {
			return ok, err
		}
		return true, recordLoss(db, "rejected", reason, 1)
	default: // unknown
		if (class == reasonCLIError || class == reasonNoStructure) && it.Attempts >= observerMaxAttempts {
			ok, err := settle(db, it, "rejected", "attempts_exhausted", nil, usage)
			if err != nil || !ok {
				return ok, err
			}
			return true, recordLoss(db, "attempts_exhausted", class, 1)
		}
		retryAt := memdNow().Add(observerBackoff(it.Attempts))
		return settle(db, it, "unknown", reason, &retryAt, usage)
	}
}

// storeObservationOnce inserts the observation, its files and the guarded 'done' settle in
// one transaction (R28; caller holds memdWriteMu). The settle guard is the lost-lease check:
// 0 rows rolls the whole transaction back. ok=false means nothing was written; otherwise the
// new observation's id is returned for the search-index hook.
func storeObservationOnce(db *sql.DB, it *queueItem, key string, o *observation, usage observerUsage) (int64, bool, error) {
	tx, err := db.Begin()
	if err != nil {
		return 0, false, fmt.Errorf("store observation: %w", err)
	}
	defer tx.Rollback()
	var project string
	err = tx.QueryRow(`SELECT project FROM sessions WHERE session_id = ?`, it.SessionID).Scan(&project)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return 0, false, fmt.Errorf("store observation project: %w", err)
	}
	facts, _ := json.Marshal(nonNil(o.Facts))
	concepts, _ := json.Marshal(nonNil(o.Concepts))
	var oid int64
	err = tx.QueryRow(`INSERT INTO observations (project, session_id, type, raw_type, title, subtitle, narrative,
		facts, concepts, prompt_version, generated_by_model, dedupe_key, source)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, 'live') RETURNING id`,
		project, it.SessionID, o.Type, o.Type, o.Title, o.Subtitle, o.Narrative,
		string(facts), string(concepts), observerPromptVersion, observerModel, key).Scan(&oid)
	if err != nil {
		return 0, false, fmt.Errorf("insert observation: %w", err)
	}
	for kind, paths := range map[string][]string{"read": o.FilesRead, "modified": o.FilesModified} {
		for _, p := range paths {
			if _, err := tx.Exec(`INSERT INTO observation_files (observation_id, path, kind) VALUES (?, ?, ?)`, oid, p, kind); err != nil {
				return 0, false, fmt.Errorf("insert observation file: %w", err)
			}
		}
	}
	ok, err := settle(tx, it, "done", "", nil, usage)
	if err != nil || !ok {
		return 0, false, err // deferred Rollback discards the observation
	}
	if err := tx.Commit(); err != nil {
		return 0, false, fmt.Errorf("store observation commit: %w", err)
	}
	return oid, true, nil
}

func nonNil(s []string) []string {
	if s == nil {
		return []string{}
	}
	return s
}

// recordHealth upserts the one-row observer_health after an observe call. since is the
// start of the current streak of the same outcome/class. Caller holds memdWriteMu.
func recordHealth(db *sql.DB, outcome, class string, now time.Time) error {
	var lastOutcome, lastReason string
	err := db.QueryRow(`SELECT last_outcome, last_reason FROM observer_health WHERE id = 1`).
		Scan(&lastOutcome, &lastReason)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return fmt.Errorf("read observer health: %w", err)
	}
	if err == nil && lastOutcome == outcome && lastReason == class {
		return nil // same streak: since stays
	}
	if _, err := db.Exec(`INSERT INTO observer_health (id, last_outcome, last_reason, since)
		VALUES (1, ?, ?, CAST(? AS TIMESTAMP))
		ON CONFLICT (id) DO UPDATE SET last_outcome = excluded.last_outcome,
			last_reason = excluded.last_reason, since = excluded.since`, outcome, class, memdTS(now)); err != nil {
		return fmt.Errorf("write observer health: %w", err)
	}
	return nil
}

// observerState reports observer health: signedOut when the last observe call was unknown
// with an auth reason, since = start of the current outcome streak (zero before any call),
// waiting = queue rows in state queued or unknown (a row mid-call is claimed and not counted).
func observerState(db *sql.DB) (signedOut bool, since time.Time, waiting int) {
	var outcome, reason, ts string
	err := db.QueryRow(`SELECT last_outcome, last_reason, CAST(since AS VARCHAR) FROM observer_health WHERE id = 1`).
		Scan(&outcome, &reason, &ts)
	if err == nil {
		signedOut = outcome == "unknown" && reason == reasonAuth
		since, _ = time.Parse("2006-01-02 15:04:05.999999", ts)
	}
	_ = db.QueryRow(`SELECT count(*) FROM queue WHERE state IN ('queued','unknown')`).Scan(&waiting)
	return signedOut, since, waiting
}
