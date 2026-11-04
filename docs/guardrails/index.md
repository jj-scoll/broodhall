# Guardrails

Every guard answers a tool call in one of four ways. The [Why](../guide/why.md) page shows a
real reply for each.

| Badge | Meaning |
| --- | --- |
| <span class="badge badge-block">BLOCK</span> | The call does not run. The reason goes back to Claude. |
| <span class="badge badge-ask">ASK</span> | The call waits for you to approve or refuse. |
| <span class="badge badge-mask">MASK</span> | The call runs, with secret or personal values masked in what Claude sees. |
| <span class="badge badge-note">NOTE</span> | The call runs, with a short note for Claude. |

## The guards

| Guard | Response | What it does |
| --- | --- | --- |
| [Bash core](bash-core.md) | <span class="badge badge-block">BLOCK</span> | Stops `rm -rf` of `/`, your home directory and a few system directories, `dd if=`, fork bombs, writes into `/etc/`, `chmod -R 777` and `chown -R` on those targets, and privileged containers. No switch. |
| [Git](git.md) | <span class="badge badge-block">BLOCK</span> <span class="badge badge-ask">ASK</span> | Blocks `git push --force` and `-f` (not `--force-with-lease`). Asks before `reset --hard`, `checkout .`, `restore .`, `clean -f`, `branch -D` and `stash`. |
| [Settings files](settings-files.md) | <span class="badge badge-ask">ASK</span> | Asks before Claude edits or writes a Claude Code `settings.json` or `settings.local.json`. No switch. |
| [Config gate](config-gate.md) | <span class="badge badge-block">BLOCK</span> | Blocks a settings change that disables this plugin, turns guard families off, or adds hooks or permissions that step around the guards, and reports new ways for data to leave the machine. No switch. |
| [Secret files](secret-files.md) | <span class="badge badge-mask">MASK</span> <span class="badge badge-ask">ASK</span> <span class="badge badge-block">BLOCK</span> | Masks a Bash read of a credentials or key file, blocks the Read tool on it, and asks before any other use or an edit. No switch. |
| [Egress](egress.md) | <span class="badge badge-ask">ASK</span> | Asks when a `gh`, `curl`, `wget`, `aws s3` or MCP write-tool call sends a secret, an outside email address or a phone number, or uploads something the guard cannot read. |
| [PII](pii.md) | <span class="badge badge-mask">MASK</span> <span class="badge badge-ask">ASK</span> <span class="badge badge-block">BLOCK</span> | Runs SQL clients through a redactor so personal values are masked in the output, blocks duckdb dot-commands that print around the mask and edits to the PII rules file, and asks before Python reads a local parquet file or a client-data bucket you configure. |
| [Session URLs](session-urls.md) | <span class="badge badge-block">BLOCK</span> | Blocks a `git commit`, `git push`, `gh pr create` or `gh pr edit` that would write or send a Claude session link. |
| [Nudges](nudges.md) | <span class="badge badge-note">NOTE</span> | Reminds Claude to plan before `terraform` or `terragrunt` `apply` and `destroy`, and to justify a `docker build`. |
| [Read limits](read-limits.md) | <span class="badge badge-note">NOTE</span> <span class="badge badge-block">BLOCK</span> | Notes a re-read of an unchanged file and a large file read; blocks a full read of a very large code file. |
| [Retry loops](retry-loops.md) | <span class="badge badge-note">NOTE</span> | Notes on the third failure of the same Bash command. |
| [Options](options.md) | | The settings that turn families off, say what to do without the binary, tell the guards which addresses and client data are yours, and opt in to the RTK rewrite. |
| [Handoff gate](handoff-gate.md) | <span class="badge badge-block">BLOCK</span> | Blocks the first stop of a session once its context passes a threshold, to get a handoff written, and blocks a second manual `/compact`. |
| [Write handoff](write-handoff.md) | | A subcommand, not a hook: `claude-tools write-handoff` writes the handoff from the transcript with a separate Haiku call on your Claude Code login. |
| [Receipt](receipt.md) | | A one-line summary at Stop of what the guards did that turn. |

## What can be turned off

The `disabled_guards` option takes a comma-separated list of families: `pii`,
`raw-client-data`, `session-url`, `egress` and `git`. The plugin's default is empty, so every
guard is on. Of the guards above, only the destructive local git
commands, Egress, PII (with `raw-client-data`, its Python-reads-data check) and Session URLs
are controlled by it.

The core blocks have no switch: Bash core, the git force-push block, Settings files and
Secret files stay on whatever `disabled_guards` says. Nudges, Read limits and Retry loops
have no family name in it either. See [Options](options.md).
