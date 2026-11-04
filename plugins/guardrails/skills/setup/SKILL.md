---
name: setup
description: Walk the user through first-time guardrails setup - pick a profile, enter their organisation's email domains, write the plugin options to settings.json after showing them, run doctor, and offer a harmless demo of a block. Safe to re-run. Use when the user runs /guardrails:setup or asks how to set guardrails up.
disable-model-invocation: true
---

# Guardrails setup

You are setting up the guardrails plugin with the user. Go one step at a time, ask before you act, and keep your messages short. Every command below runs the plugin's own binary through its launcher. Quote the launcher path exactly as shown.

This command is safe to run again. The setup subcommand only touches the options you give it, shows the old and new value of each one, and writes nothing when nothing changes.

## 1. Check the binary

Run:

```sh
sh "${CLAUDE_PLUGIN_ROOT}/scripts/run.sh" version
```

If it prints a version, go on. If it prints nothing, the binary is not installed yet (the launcher stays silent when it is missing). Run `sh "${CLAUDE_PLUGIN_ROOT}/scripts/run.sh" install`, then run `version` again. If it still prints nothing, show the user the install output and stop: setup cannot continue without the binary.

## 2. Choose a profile

Ask the user to pick one. A profile only sets plugin options that already exist. It never turns a guard family off.

| Profile | What it does |
| --- | --- |
| `standard` | Every guard family on. The guards stop or ask, as described in the docs. |
| `observe` | Every guard family on, but the optional guards only log what they would have asked or blocked. Good for the first few days. The user can run `guard-report` through the plugin launcher later to see the counts. |

If they want a family off, tell them to use `/plugin configure guardrails@broodhall` themselves. That is a deliberate choice, and the config gate refuses it when it is made in a running session.

## 3. Ask for their email domains

Ask for the email domains or addresses that belong to the user's organisation, comma-separated, such as `acme.com, dev@partner.org`. The egress guard treats these as the user's own, so it does not ask each time a command mentions one of them. A bare domain becomes `@domain`. The domain of the global git `user.email` always counts, so an empty answer is fine. Skip this step if they have nothing to add.

## 4. Show what will be written

Build the command from their answers. Leave out `-domains` when they gave none. Run it without `-write` first:

```sh
sh "${CLAUDE_PLUGIN_ROOT}/scripts/run.sh" setup -profile standard -domains acme.com
```

Use `-profile observe` for the observe profile. The output names the settings file, then lists each option with its old and new value, and ends with `dry run: nothing written`. If it says `already up to date`, nothing needs writing: go to step 6.

Show the user that output as it is. Say which file it is (the user settings file, under `CLAUDE_CONFIG_DIR` when set) and that the previous contents are kept beside it in `settings.json.guardrails-setup.bak`. Ask whether to write it. Do not write without a clear yes.

## 5. Write it

On a yes, run the same command with `-write` added:

```sh
sh "${CLAUDE_PLUGIN_ROOT}/scripts/run.sh" setup -profile standard -domains acme.com -write
```

Two things happen here, and both are expected.

1. The guardrails settings-file guard asks the user to approve this command, because `setup -write` changes `settings.json`. Tell the user to approve it only if it matches the preview they just saw.
2. After the write, config-gate may report `blocked the change to settings.json` and say this session keeps its current settings. That happens when the change turns observe mode on or adds an email domain, since both loosen a guard. It is not a failure: the file is written, and the new options apply from the next session. Tell the user to start a new session.

If setup reports an error, such as a settings file that is not valid JSON, is read-only, or has content after its JSON value, show the message and stop. Do not edit the file by hand to get around it.

## 6. Run doctor

```sh
sh "${CLAUDE_PLUGIN_ROOT}/scripts/run.sh" doctor
```

Read the output to the user. Every line starting `ok` is fine. A `WARN` or `FAIL` line says what is wrong and what to run. Explain it in plain words. The most common warnings are plugins at different versions (run the `claude plugin update` command it prints) and a statusLine that still runs the old `claude-tools` binary.

## 7. Optional: PII column rules

If the user works with SQL on data that holds people's details, mention that the PII mask has column-name rules they can edit. `guardrails pii-rules init` writes the built-in rules to a file they own and never overwrites an existing one. Claude is not allowed to edit that file, so do not run it. Offer this line for the user to type themselves, with the `!` prefix:

```text
! sh "${CLAUDE_PLUGIN_ROOT}/scripts/run.sh" pii-rules init
```

## 8. Offer a demo

Offer to show a block working. The command below copies nothing from `/dev/null` to `/dev/null`, so it is harmless, but the core `dd` guard blocks every `dd` with an `if=` argument:

```sh
dd if=/dev/null of=/dev/null count=0
```

If the user agrees, run exactly that command with the Bash tool. Expected result: the call does not run, and the reply reads `BLOCKED: 'dd if=' is a high-risk destructive operation`. Tell the user that this is the guard working, and that a block is a message to Claude, not an error to work around. If the command ran with no block, the guards are not active: tell the user, and point them to the Troubleshooting page of the docs.

## 9. Finish

Summarise in three lines: the profile and domains written, the doctor result, and whether the demo was blocked. Remind the user that re-running `/guardrails:setup` is safe, and that the docs are at https://jj-scoll.github.io/broodhall/.
