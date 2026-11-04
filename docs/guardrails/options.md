# Options

Three plugin options, two files under `~/.config/claude-tools/`, and one environment variable.

Set the plugin options with `/plugin configure guardrails@broodhall`, or from `/config`.

## `disabled_guards`

| | |
| --- | --- |
| Title | Disabled guard families |
| Default | `pii,raw-client-data,session-url` |
| Description | Comma-separated families to turn off: pii, raw-client-data, session-url, egress, git. The core blocks (rm -rf of home or root, git push --force, dd, fork bombs) cannot be turned off. |

The five family names, and what each one controls:

| Family | Controls |
| --- | --- |
| `git` | The asks before `git reset --hard`, `checkout .`, `restore .`, `clean -f`, `branch -D` and `stash`. See [Git](git.md). |
| `egress` | The asks when a command sends a secret, an outside email address or a phone number. See [Egress](egress.md). |
| `pii` | The SQL-output mask, the block on duckdb dot-commands and the block on edits to the PII rules file. See [PII](pii.md). |
| `raw-client-data` | The ask before Python reads a local parquet file. See [PII](pii.md#raw-client-data-ask). |
| `session-url` | The block on a push or PR that carries a session link. See [Session URLs](session-urls.md). |

Core blocks have no switch. [Bash core](bash-core.md), the `git push --force` block,
[Settings files](settings-files.md) and [Secret files](secret-files.md) stay on whatever
this option says. [Nudges](nudges.md), [Read limits](read-limits.md) and
[Retry loops](retry-loops.md) have no family name either.

Names are separated by commas, and spaces around a name are ignored. A name that is not one of
the five has no effect. An empty value turns every family on. The launcher falls back to the
`CLAUDE_TOOLS_OFF` environment variable, then to the three default names. The binary run on
its own treats an unset `CLAUDE_TOOLS_OFF` as every family on.

## `allowed_email_domains`

| | |
| --- | --- |
| Title | Your email addresses or domains |
| Default | empty |
| Description | Comma-separated addresses or @domains the egress guard treats as yours, such as @example.com. The domain of your global git user.email always counts. |

Outside the plugin the same list is read from `CLAUDE_TOOLS_PII_ALLOW`.

- In [Egress](egress.md), a full address matches that address, and `@example.com` matches the
  domain and its subdomains. The domain of `git config --global user.email` always matches.
- In the [PII](pii.md) mask, only full addresses stay readable (and your git address). An
  `@domain` entry has no effect there.

## `handoff_tokens`

| | |
| --- | --- |
| Title | Handoff threshold (tokens) |
| Default | empty |
| Description | Context size at which the Stop gate asks Claude, once per session, to write a handoff. Leave empty for 80% of CLAUDE_CODE_AUTO_COMPACT_WINDOW (240000 for the default 300000 window). |

Outside the plugin the same number is read from `HANDOFF_TOKENS`. A value that is not a
whole number counts as empty. See [Handoff gate](handoff-gate.md).

## `~/.config/claude-tools/secret-paths`

Extra files for [Secret files](secret-files.md) to protect, one path or glob per line. The
list adds to the built-in ones; it does not replace them. The file itself is protected too.

```text
# my deploy credentials
~/.config/myapp/credentials.toml
~/work/*/.env.production
```

- `~` is expanded to your home directory. Blank lines are skipped, and so are lines that start
  with `#`. A `#` after text on a line is part of the path.
- A line that is not an absolute path after `~` is expanded names no file and is ignored.
- If `XDG_CONFIG_HOME` is set to an absolute path, the file is read from
  `$XDG_CONFIG_HOME/claude-tools/secret-paths` instead.
- The file is read on each tool call, so a new line applies at once.

## `~/.config/claude-tools/pii-columns.json`

The column rules for the [PII](pii.md) mask. It is used only when the `pii` family is on. The
location follows the same `XDG_CONFIG_HOME` rule as above. Claude is blocked from writing it.

The file is JSON with two keys that matter. Other keys, such as `notes`, are ignored.

```json
{
  "rules": [
    {
      "name": "email",
      "category": "email",
      "tier": 1,
      "pattern": "(?:[a-z0-9_]*_)?e_?mail(?:_?addr(?:ess)?)?",
      "exclude": ".*_(?:enabled|flag|verified|sent|count|type|status|template|subject|opt_?in)"
    }
  ],
  "explicit_columns": ["driver_id"]
}
```

- `name` identifies the rule. `category` is the tag in the masked cell (`[email:3229ba]`) and
  must be one of the categories the built-in rules use: `address`, `address_coarse`,
  `credentials`, `demographic`, `dob_age`, `email`, `financial`, `financial_policy_claim`,
  `free_text`, `gov_id`, `health_info`, `ip_device`, `json_blob`, `other_identifier`,
  `person_name` and `phone_fax`. A file with any other category is rejected.
- `pattern` is a regular expression matched against the whole column name after it is
  lowercased and converted from camelCase to snake_case (a trailing number is also tried
  without, so `email2` meets the `email` rule). It is anchored: it must match the whole name.
  `exclude`, if present, is a second pattern that stops a match.
- `tier` decides whether a rule is used. Tier 1 rules always are. Of the tier 2 rules, only
  those named `policy_claim_number`, `vin` and `demographic` are. All other rules are skipped.
- `explicit_columns` lists column names to mask as `[key:tag]`, by exact name.

Email and phone shapes in any value are masked whatever the file says.

Your file replaces the built-in rules. It does not add to them. If it is missing, unreadable,
not valid JSON, has a bad pattern or an unknown category, or has no active rule, the built-in
rules are used and nothing is reported at mask time. To start from the built-in rules and
edit them, run `claude-tools pii-rules init`, which writes the file and will not overwrite an
existing one. To check a file, run `claude-tools pii-rules check [path]`:

```text
ok /home/you/.config/claude-tools/pii-columns.json: 28 rules active (33 in file), 1 explicit columns
```

```text
invalid bad.json: pii-columns.json: rule 0 ("x"): unknown category "bogus"
```

## `CLAUDE_TOOLS_BIN`

The hooks run one binary. By default that is the copy the plugin downloads for its version, to
`bin/<version>/claude-tools` under the plugin's data directory. Set `CLAUDE_TOOLS_BIN` to the
path of your own build to run that instead, for example one you built from this repository.

The download is skipped only when the path is executable. If the path is not an executable
file, the session-start step downloads the release copy anyway, but the hooks still look
at your path, find nothing to run, and let every call through, so the guards are off. Check the
path before you rely on it.
