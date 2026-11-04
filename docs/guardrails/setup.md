# Setup

`/guardrails:setup` walks you through first-time setup. It is safe to run again.

!!! why "Why the command is /guardrails:setup"
    A slash command made by a plugin is named after the plugin: `/<plugin>:<skill>`. broodhall
    is the name of the marketplace, not of a plugin, so there is no `/broodhall:setup`. The
    command lives in the guardrails plugin, because the options it writes are guardrails
    options.

Run it in a session after you install the plugin:

```text
/guardrails:setup
```

Claude asks four things, one at a time. You can stop at any step.

1. **A profile.** `standard` leaves every guard family on. `observe` leaves them on but only
   logs what the optional guards would have asked or blocked, which suits the first few days.
   A profile never turns a family off. To do that, use `/plugin configure guardrails@broodhall`
   yourself. The [config gate](config-gate.md) refuses that change when it is made inside a
   running session.
2. **Your email domains.** The [egress guard](egress.md) does not ask about addresses at your
   own domains. Enter them comma-separated, such as `acme.com, dev@partner.org`. A bare domain
   is stored as `@acme.com`. The domain of your global git `user.email` always counts, so you
   can leave this empty.
3. **Whether to write.** Claude first runs the setup subcommand without `-write`. It shows the
   settings file, then each option with its old and new value. Nothing is written until you
   say yes.
4. **A demo.** Claude runs `dd if=/dev/null of=/dev/null count=0`. It copies nothing, and the
   [core guard](bash-core.md) blocks every `dd` with an `if=` argument, so you see
   `BLOCKED: 'dd if=' ...` without any risk.

Between steps 3 and 4 it runs `guardrails doctor` and explains any warning. If you work with
SQL on personal data, it also shows you the `guardrails pii-rules init` line to type
yourself. Claude does not run that one, because the [PII rules](pii.md) file is yours to edit.

## What it writes

The options go to `pluginConfigs` in your user settings file, `~/.claude/settings.json` (under
`CLAUDE_CONFIG_DIR` when set). Claude Code reads `pluginConfigs` only from user and managed
settings, never from a project file. This is what `/plugin configure` writes too.

```json
{
  "pluginConfigs": {
    "guardrails@broodhall": {
      "options": {
        "disabled_guards": "",
        "observe": false,
        "allowed_email_domains": "@acme.com"
      }
    }
  }
}
```

- Only the options you answered are touched. Every other key in the file stays.
- The marketplace name comes from your `enabledPlugins` entry for guardrails, else it is
  `broodhall`.
- The file is rewritten with its keys sorted and two-space indentation. The previous contents
  are kept in `settings.json.guardrails-setup.bak`. A run that changes nothing writes nothing
  and makes no backup.
- A `settings.json` that is a symlink is written through, and the link stays. A symlink to a
  file that does not exist is left alone.
- A file that is not valid JSON, holds anything but one JSON object, or has content after the
  object is left alone. So is a read-only file.
- Run one setup at a time. Two runs at once can lose one of the two changes.

## Asking and config-gate

`setup -write` changes your settings file without naming it, so the guardrails
settings-file guard treats it like any other model write to `settings.json` and asks you to
approve it. This holds for the plugin launcher and for the binary under either name.

[Config-gate](config-gate.md) also treats two of these options as loosening a guard, the way
it treats `disabled_guards`: turning `observe` on, and adding an entry to
`allowed_email_domains`. When setup does either, config-gate may report `blocked the change`
and keep the running session on its old settings. The file is written, and the options apply
from your next session. Turning `observe` off, removing a domain, and the `standard` profile
raise protection and pass silently.

## From a terminal

The skill runs the `setup` subcommand through the plugin launcher. You can run it by hand:

```sh
guardrails setup -profile standard -domains acme.com          # show the change
guardrails setup -profile standard -domains acme.com -write   # write it
```

| Flag | Meaning |
| --- | --- |
| `-profile standard\|observe` | Sets `disabled_guards` to empty, and `observe` off or on. Left out: those options stay as they are. |
| `-domains a,b` | Sets `allowed_email_domains`. Left out: it stays as it is. |
| `-marketplace NAME` | The marketplace name, when it is not `broodhall` and `enabledPlugins` does not name it. |
| `-config-dir DIR` | The Claude config directory. Default: `CLAUDE_CONFIG_DIR`, else `~/.claude`. |
| `-write` | Write the change. Without it setup only shows it. |

Exit status is 0 on success, 1 when the file cannot be read, parsed or written, and 2 for bad
flags.

## If nothing happens

The launcher stays silent for this command when the guard binary is not installed yet. The
skill checks with `version` first and runs `install` if needed. See
[Troubleshooting](../guide/troubleshooting.md).
