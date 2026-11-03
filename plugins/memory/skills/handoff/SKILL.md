---
name: handoff
description: Write a handoff so a brand-new Claude session can pick up unfinished work without re-deriving anything. Use when the user says they are stopping, switching sessions or machines, or asks for a handoff, and when this session has grown long or is looping on the same failing approach.
---

# Handoff

A handoff is a note from you to a reader who remembers nothing. The reader is a fresh session that will see the note once, at startup, and then has to continue the work. Everything it needs must be in the note or one hop away from it.

The plugin stores a handoff in the memory work state, in a list named `handoff`. At the next session start in the same project, the memory plugin shows the handoff at the top of the context, one time only, and marks it as shown.

## When to write one

- The user is stopping, going to another machine, or says "hand this off", "save where we are" or similar.
- The session is long and the context is crowded, so a clean restart with a good note would beat pushing on.
- You have attempted the same fix more than twice, or you notice you are repeating yourself. Writing down what you tried is often the way out, even if you then keep working.
- Work is half-done and the user will clearly come back to it.

Do not write one for finished work with nothing left to do; the memory plugin already records what happened.

## Gather the facts first

Before writing, check the real state instead of recalling it:

1. Run `git status` and `git log --oneline -10` in each repo you touched. Note the branch, uncommitted files, and the last commit.
2. Re-read any task list you keep (`work_state_read`) so the note agrees with it.
3. Find the exact command that shows the current failure or the current success, and run it once if it is cheap. Copy the real output line that matters.

## What the note must contain

Choose headings that fit the work, but cover these seven things. Leave out any that are empty; a short accurate note beats a padded one.

- **Goal.** One or two sentences: what outcome the user wants, in their terms, and why it matters.
- **Where things stand.** What is done and verified, what is half-done, what is untouched. Say how you know (a test passed, you ran the command, or you only read the code).
- **Next action.** The very first thing the reader should do, concrete enough to start without thinking: a command, a file and function, a question to put to the user.
- **Dead ends.** Each approach that failed, with the reason it failed. Without this the reader will try them again. Include approaches the user vetoed and their stated reason, marked as vetoed.
- **Decisions and constraints.** Choices already made and the rules the reader must keep following (style, versions, things not to touch, environments that are off limits).
- **Locations.** Absolute paths of files you changed or that matter, branch names, PR or ticket links, ports, job or run IDs, and any generated artifact the work produced. Record the exact values; do not describe them.
- **Open questions.** Anything you were unsure of or waiting on, labelled as a guess where it is one.

Writing rules:

- Be specific. "Fix the flaky test" is useless; "`TestQueueDrain` in `internal/queue/drain_test.go` fails about 1 run in 5 with a timeout at line 88; I suspect the unbuffered channel at line 61" can be acted on.
- Quote numbers, error text, IDs and commands exactly as they appear.
- Separate what you verified from what you inferred.
- Never put secrets, tokens or credentials in the note. Point to where they live instead.
- Keep the saved digest under about 3,000 characters; the startup display cuts longer text.

## How to save it

Use two places, because the startup display is short and the full note can be long.

**1. Write the full note to a file.** Create `HANDOFF.md` at the root of the project (or another path if the user prefers) with the complete content from the section above. Use the Write tool.

**2. Save a digest in memory.** Call the memory MCP tool `work_state_write`, which Claude Code exposes as `mcp__memory__work_state_write`, with:

- `list`: `"handoff"`
- `fields`:
  - `text` (required): the digest, plain text with short lines. Put the goal, the state, and the next action first, since a long text is cut from the end.
  - `handoff_file`: the absolute path of the file from step 1. The startup display tells the reader to find the full text there.
  - `obs_ids`: optional, a comma-separated string of memory observation IDs (from `search` or `timeline`) that hold useful background. The display tells the reader to open them with `get_observations`.

Example call:

```json
{
  "list": "handoff",
  "fields": {
    "text": "Goal: move the queue worker to batch commits.\nDone: batching in worker.go, unit tests pass (ran go test ./internal/queue).\nNext: run the 10k-row benchmark in bench/queue_test.go and compare with the 41s baseline.\nDead end: raising the batch size above 500 deadlocks the writer.\nAvoid: do not touch the migration files.",
    "handoff_file": "/home/me/src/app/HANDOFF.md",
    "obs_ids": "412,415"
  }
}
```

The reply says the handoff was stored for the project. If the call fails or the tool is missing, the `HANDOFF.md` file is still the record; tell the user the memory save failed and where the file is.

## What the next session sees

- It appears once, at the top of the startup context, under the heading "Handoff left by an earlier session" with its age.
- After a `/clear` or a normal start it is shown if it is newer than the age limit (7 days unless the memory settings say otherwise). On a resumed session it is not shown or consumed. After a compaction it is only shown again if it was written in the last 30 minutes.
- A newer handoff replaces an older one that was never shown. Once shown, it will not appear again, so the file is the durable copy.

## Finish by telling the user

Say that the handoff is saved, give the path of `HANDOFF.md`, and quote the "Next action" line, so they can see at a glance whether it matches what they expect.
