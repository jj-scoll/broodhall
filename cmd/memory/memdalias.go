package main

import (
	"database/sql"
	"log"
	"strings"
	"sync"
)

// Project aliases let a project key read the rows an earlier daemon wrote under the old
// basename key. project_aliases(old_key, new_key) is the whole mechanism: reads for new_key
// also match rows whose project is old_key; no row is ever rewritten, and DROP TABLE
// project_aliases undoes it. Reads are only widened, never narrowed: asking for an old key
// returns just that key.

// projectKeys returns project followed by every old key aliased to it ("" gives nil). A missing
// or unreadable alias table gives just the project.
func projectKeys(db *sql.DB, project string) []string {
	if project == "" {
		return nil
	}
	keys := []string{project}
	rows, err := db.Query(`SELECT old_key FROM project_aliases WHERE new_key = ? ORDER BY old_key`, project)
	if err != nil {
		return keys
	}
	defer rows.Close()
	for rows.Next() {
		var k string
		if rows.Scan(&k) == nil && k != project {
			keys = append(keys, k)
		}
	}
	return keys
}

// projectMatch renders "col = ?" or "col IN (?, ...)" for keys; no keys match the empty project.
func projectMatch(col string, keys []string) (string, []any) {
	if len(keys) == 0 {
		keys = []string{""}
	}
	args := make([]any, len(keys))
	for i, k := range keys {
		args[i] = k
	}
	if len(keys) == 1 {
		return col + " = ?", args
	}
	return col + " IN (" + strings.TrimSuffix(strings.Repeat("?,", len(keys)), ",") + ")", args
}

// projAnd renders " AND <match>" for project and its aliases, or "" for an empty project.
func projAnd(db *sql.DB, col, project string) (string, []any) {
	keys := projectKeys(db, project)
	if len(keys) == 0 {
		return "", nil
	}
	c, a := projectMatch(col, keys)
	return " AND " + c, a
}

// aliasMemo remembers a decision for one (database, old key, new key) for the process lifetime.
type aliasMemo struct {
	db       *sql.DB
	old, new string
}

var aliasDecided sync.Map

// aliasMaxCwds bounds how many distinct session directories one decision inspects.
const aliasMaxCwds = 1000

// ensureProjectAlias records old key -> new key for the project that cwd resolves to, when rows
// exist under the old basename key and they plausibly belong to this directory's repo. The
// decision, made once per process and pair:
//
//   - no rows under the old key: nothing to alias
//   - some session under the old key has a cwd outside this project's root: the key is shared
//     with another directory and the rows cannot be split, so no alias is made
//   - otherwise (every session cwd is inside the root, or no session carries a cwd at all,
//     as with manual notes and imports): alias
//
// Failures are logged and retried on the next call; reads work without the alias.
func ensureProjectAlias(db *sql.DB, cwd string) { ensureAlias(db, cwd, withWrite) }

// ensureAlias is ensureProjectAlias with the write serialiser chosen by the caller: the tailer
// already holds memdWriteMu (not reentrant) and passes a plain call.
func ensureAlias(db *sql.DB, cwd string, write func(func() error) error) {
	p := resolveProject(cwd)
	if p.key == p.legacy || p.legacy == "" || p.legacy == memdUnknownProject {
		return
	}
	memo := aliasMemo{db, p.legacy, p.key}
	if _, done := aliasDecided.Load(memo); done {
		return
	}
	want, err := aliasWanted(db, p)
	if err != nil {
		log.Printf("memory: project alias %q -> %q: %v", p.legacy, p.key, err)
		return
	}
	if want {
		err = write(func() error {
			_, e := db.Exec(`INSERT INTO project_aliases (old_key, new_key) VALUES (?, ?) ON CONFLICT DO NOTHING`, p.legacy, p.key)
			return e
		})
		if err != nil {
			log.Printf("memory: project alias %q -> %q: %v", p.legacy, p.key, err)
			return
		}
	}
	aliasDecided.Store(memo, true)
}

func aliasWanted(db *sql.DB, p memdProj) (bool, error) {
	var has bool
	if err := db.QueryRow(`SELECT EXISTS (SELECT 1 FROM observations WHERE project = $1)
		OR EXISTS (SELECT 1 FROM summaries WHERE project = $1)
		OR EXISTS (SELECT 1 FROM work_state WHERE project = $1)
		OR EXISTS (SELECT 1 FROM sessions WHERE project = $1)`, p.legacy).Scan(&has); err != nil || !has {
		return false, err
	}
	rows, err := db.Query(`SELECT DISTINCT cwd FROM sessions WHERE project = ? AND cwd <> '' LIMIT ?`, p.legacy, aliasMaxCwds)
	if err != nil {
		return false, err
	}
	defer rows.Close()
	roots := []string{p.root}
	if r := memdRealpath(p.root); r != p.root {
		roots = append(roots, r)
	}
	for rows.Next() {
		var c string
		if err := rows.Scan(&c); err != nil {
			return false, err
		}
		if !underAny(c, roots) && !underAny(memdRealpath(c), roots) {
			return false, nil
		}
	}
	return true, rows.Err()
}

func underAny(dir string, roots []string) bool {
	for _, r := range roots {
		if dir == r || strings.HasPrefix(dir, strings.TrimRight(r, "/")+"/") {
			return true
		}
	}
	return false
}
