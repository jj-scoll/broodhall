# Read limits

<span class="badge badge-note">NOTE</span> on a re-read of an unchanged file, and on a large file read.
<span class="badge badge-block">BLOCK</span> a whole-file read of a very large code file.

!!! why "Why it exists"
    Everything Claude reads takes room in the conversation, and the conversation is sent
    again on every turn. Reading a large file whole, or reading the same unchanged file
    twice, spends that room on text Claude often does not need. So the guard reminds Claude of
    a read it already made, and steers a whole-file read of a big code file toward a search
    for the part it wants and a read of just that range.

## What it catches

Two hooks run on every `Read`: `read-dedup` and `large-read-guard`.

### A re-read of an unchanged file: NOTE

Each read of a file is recorded. A read of the same file, with the same offset and limit,
while the file's modification time is unchanged and within 30 minutes of the previous read,
gets a note. The read still runs.

```text
You already read /home/you/proj/server.py at 13:39 this session and it is unchanged; the content is likely still in context above. Re-reading repeats roughly 3075 tokens — skip unless you need a fresh look.
```

The time is when the file was last read, in your local time. The token figure is the file's
size divided by four. A read with a different offset or limit is a different read, and so is
a read after the file changed. After a compaction the guard forgets the session's reads (the
compact-guard hook clears them), so the first read of a file after a compaction gets no note.

### A large read: NOTE, then BLOCK

This applies to a `Read` that gives no `limit`. The guard estimates what the read would put in
the conversation: the size of the first 2000 lines of the file (all a whole-file `Read`
returns) in bytes, divided by four.

| Estimate | Answer |
| --- | --- |
| 2,000 tokens or fewer | silent |
| More than 2,000 | **NOTE**. The read runs. |
| More than 10,000 | **BLOCK**. The read does not run. |

In bytes of the first 2000 lines, the note starts at about 8,000 and the block at about 40,000.

```text
Large read: /home/you/proj/server.py is 300 lines (~3075 tokens). Consider Read with offset+limit if you only need a specific range. Proceeding.
```

```text
Read blocked: /home/you/proj/engine.py would inject ~12300 tokens (1200 lines) into context. Use Grep to locate the relevant section, then Read with offset+limit for the ~20-50 lines you need. (large-read-guard hook)
```

The same hook is where the [Secret files](secret-files.md) guard blocks a `Read` of a
credentials file.

## What passes

- A `Read` that gives a `limit`: it is already a targeted read. An `offset` alone does not
  count, so a whole-file `Read` with only an offset still gets the estimate.
- Files that are not code: `md`, `markdown`, `json`, `yaml`, `yml`, `toml`, `ini`, `cfg`,
  `conf` and `txt`, and images (`png`, `jpg`, `jpeg`, `gif`, `webp`), which cost by size in
  pixels and not by bytes.
- A file about 8,000 bytes or smaller (in its first 2000 lines), and a file that is missing or
  cannot be read.
- A second read after the file changed, or with a different range.

## Turning it off

You cannot. Read limits have no family name in `disabled_guards`, and none of the five
families (`pii`, `raw-client-data`, `session-url`, `egress`, `git`) controls them. The note
never blocks; the block has a way around it, which is the one it names: read the range you
need with `offset` and `limit`. You can disable the plugin. See [Options](options.md).
