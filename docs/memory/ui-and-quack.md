# UI and Quack

The daemon serves two local interfaces. Both listen on `127.0.0.1` only, so other machines
cannot reach them.

## The UI

Open `http://127.0.0.1:9495/` in a browser while the daemon runs. It is a web page served by
the daemon itself, for browsing what memory has stored. If the port is busy, the daemon logs `memory ui: not serving` and continues.

## Quack

Quack is a DuckDB extension that serves a database over the network. The daemon uses it to
let a DuckDB client open the memory database while the daemon has it locked. It listens on
`127.0.0.1:9494` and needs a token. The token is the content of `quack.token` in the state
directory (`~/.local/share/memory/quack.token` by default). The daemon creates the file with
mode 0600 the first time it runs.

From a DuckDB shell with the `quack` extension available:

```sql
ATTACH 'quack:localhost:9494' AS q (TOKEN '<contents of quack.token>');
SHOW ALL TABLES;
```

Quack has no read-only mode, so the daemon adds its own check. It allows a statement only if
it starts with `SELECT`, `WITH`, `FROM`, `DESCRIBE`, `SHOW`, `SUMMARIZE` or `EXPLAIN` (or is
a bare `BEGIN`, `COMMIT` or `ROLLBACK`) and contains no word for a write, a schema change, a
setting, a file read or an extension. A query that merely mentions one of those words in a
string is refused too. Anything refused fails with `memory quack is read-only: statement
refused`.

The token keeps other programs on your machine from reading the database. Do not copy it into
scripts that you share. If the file is deleted or has the wrong permissions, the daemon
makes a new one at its next start.

If the port is busy, or the extension cannot load, the daemon logs `memory quack: not
serving` and continues without it. Everything else works.
