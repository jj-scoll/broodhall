# PII

<span class="badge badge-mask">MASK</span> personal values in the output of SQL clients.
<span class="badge badge-block">BLOCK</span> a duckdb command that would print around the mask, and edits to the PII rules file.
<span class="badge badge-ask">ASK</span> before Python reads a local parquet file, or a client-data S3 bucket you configure.

This guard is **on by default**, like every family. See [Turning it off](#turning-it-off) to
turn it off.

!!! why "Why it exists"
    Whatever Claude reads goes into the conversation and the transcript. A query result with
    names, email addresses or phone numbers in it leaves that data in places you do not
    control, and Claude rarely needs the people, only the shape of the data and which rows
    join to which. So a SQL client's output goes through a redactor that swaps personal
    values for a short tag, and equal values get equal tags, so a join still reads. The rules
    that decide what counts as personal are yours to edit, so Claude is not allowed to loosen
    the masking by rewriting them.

## What it catches

### SQL output: MASK

With the guard on, these commands are rewritten to run under `claude-tools redact --pii`:

- `psql`, forced to `--csv` so the redactor sees column names, then printed in the format you
  asked for (`-t`, `-A`, `-F` are restored).
- `duckdb`, every call that runs SQL, forced to `-json` and restored to the output mode you asked
  for. With no mode flag it prints markdown and keeps duckdb's agent-mode row and cell limits.
- `aws rds-data execute-statement` (with `--include-result-metadata` added) and
  `aws athena get-query-results`.

For `psql -h db -c "select email from users"`, Claude's call becomes:

```text
claude-tools redact --pii --private --in=csv --to=aligned --footer -- psql --csv -h db -c "select email from users"
```

(The reply carries the full path of the binary in place of `claude-tools`.) The redactor
masks:

- Cells in columns whose names match the PII rules: email, phone, person name, address,
  postal code, date of birth and age, government ids, bank and card numbers, credentials, IP
  and device ids, and similar. The cell becomes `[category:tag]`.
- Email and phone shapes in any string, such as inside a free-text `notes` cell.

The tag is six hex digits of a keyed hash of the value. The key is a random local file
(by default `~/.local/state/claude-tools/pii-key`, created on first use), so tags differ per machine and
equal values share a tag. Empty and `NULL` cells stay as they are. Columns that match no rule,
such as `id`, `amount` and the coarse `city`, pass through.

Output shown to Claude is wrapped in `<private>` and `</private>` lines, at most 50 lines per
block, which a note-taking tool can use to leave the block out (a command whose output is piped
or redirected gets the mask without the wrapper). This is what
`redact --pii` prints for a table with an email and a first name:

```text
<private>
 id | email          | first_name           | city
----+----------------+----------------------+--------
 1  | [email:d1a406] | [person_name:6e6d8b] | Ottawa
(1 row)
</private>
```

Fed CSV with a free-text cell, it masks the shapes inside it:

```text
id,email,notes,city,amount
1,,call [phone:00a45b] re [email:529ae8],Ottawa,5
2,NULL,x,Hull,6
```

Addresses you own stay readable: your global `git config user.email`, and the full addresses
in `CLAUDE_TOOLS_PII_ALLOW` (the `allowed_email_domains` option). An `@domain` entry is
honored by [Egress](egress.md) but not here: the mask only skips exact addresses.

Gaps: SQL run inside `bash -c`, a script, `ssh` or `ssm`; a second result set in one
`psql -c`, which gets value masking only; and output written to a file and read later.

### duckdb dot-commands: BLOCK

A duckdb command with `.mode`, `.output`, `.once`, `.read`, `.shell`, `.system` or `.excel`
would change how or where results print, around the mask.

```text
BLOCKED: duckdb .mode changes how or where results print, so PII masking can't apply. Use flags instead: -csv, -line, -list, -markdown, -noheader, -separator S; COPY (...) TO 'file' to write a file; put the SQL of a .read file inline (-c or a heredoc).
```

### The PII rules file: BLOCK

The rules live in `~/.config/claude-tools/pii-columns.json` (see [Options](options.md)).
Claude cannot change them: an Edit, Write, MultiEdit or NotebookEdit on that file, a Bash
command that writes it (a redirect, `sed -i`, `tee`, `mv`, `rm`, `cp` onto it, an editor, an
interpreter near a write), and a Bash command that sets `HOME` or `XDG_CONFIG_HOME` while
running `claude-tools` are all blocked. Reading the file passes.

```text
BLOCKED: PII rules (~/.config/claude-tools/pii-columns.json) are the user's to edit, not the model's.
```

### Raw client data: ASK

The mask only covers SQL clients, because it needs the column names. Python that reads data
prints it unmasked. So, as its own family, `raw-client-data`, the guard asks before Python
reads raw client data: a `python`, `python3.x`, `ipython` or `uv run python` command whose
text names a local `.parquet` path, or an `s3://` object in a bucket you listed.

```text
ASK: Python reading raw client data (claims.parquet): its output is not PII-masked. Prefer the duckdb CLI, whose output is masked. Confirm with the user before running it.
```

You approve or refuse. This check reads the command text only: a script that opens the
file by itself is not seen, and a parquet path that is a URL is not checked unless it is an
S3 object in a configured bucket.

#### Your client data

Two options say what is client data in your setup. Nothing is configured by default, so
without them only local parquet paths ask and no path is exempt.

- `client_data_buckets` (`CLAUDE_TOOLS_CLIENT_BUCKETS` outside the plugin): comma-separated
  S3 bucket names, where `*` matches any run of name characters. With
  `*-clientdata, acme-raw`, Python text that names `s3://acme-prod-clientdata/exports/` or
  `s3://acme-raw/a.parquet` asks, and `s3://acme-own/a.parquet` does not.
- `masked_copy_dirs` (`CLAUDE_TOOLS_MASKED_DIRS`): comma-separated directories that hold
  masked copies of the data. A local parquet path that contains one does not ask. With
  `~/.cache/masked/`, `python3 q.py ~/.cache/masked/acme/x.parquet` passes and
  `python3 q.py exports/x.parquet` still asks. A leading `~` matches any home directory.

These do not change the `duckdb` rules above: a `duckdb` command is never a Python read.

## What passes

- Everything, when `pii` is in `disabled_guards`: `psql -h db -c "select email from users"`
  runs as typed. The same goes for a `duckdb` dot-command and for edits to the rules file.
- Commands that are not SQL clients, and duckdb runs that print no results (`--version`,
  `-help`, `-ui`).
- `duckdb -c "select * from read_parquet('claims.parquet')"`, which runs under the mask.
- `echo "claims.parquet"` and any command without Python in it.
- A parquet path under a `masked_copy_dirs` directory, and an S3 bucket that
  `client_data_buckets` does not name.
- The Raw client data ask passes when `raw-client-data` is in `disabled_guards`, even with
  `pii` on, and the reverse also holds.

This is a speed bump, not a wall. Claude runs as you, and a script written earlier reaches
past any of these checks.

## Turning it off

`pii` and `raw-client-data` are two families in `disabled_guards`, and the plugin's default
list is empty, so both are on until you change it. Add `pii` to turn the mask, the
dot-command block and the rules-file block off. Add `raw-client-data` to turn off the Python
parquet ask. See [Options](options.md).
