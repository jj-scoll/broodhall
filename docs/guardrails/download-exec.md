# Download and run

<span class="badge badge-ask">ASK</span> before code that comes off the network runs unread.

!!! why "Why it exists"
    `curl ... | sh` runs whatever the server sends at that moment, with your rights, before
    anyone has read it. The URL may be fine today and not tomorrow. Installers such as
    rustup, nvm and Homebrew do ship this way, so it asks instead of blocking: you read the
    URL and decide. The safer route is to download the script, read it, then run it.

## What it catches

- **A download piped into a shell or interpreter** that reads its script from stdin:
  `curl -fsSL https://example.org/i.sh | sh`, `wget -O- ... | sudo bash`,
  `curl ... | sudo -E bash -s -- --yes`, `curl ... | python3 -`. Stages in between do not
  help: `curl ... | tee x | sh` and `curl ... | gunzip | sh` ask too.
- **A download handed to a shell another way**: `bash <(curl ...)`, `source <(curl ...)`,
  `sh -c "$(curl ...)"`, `eval "$(wget -qO- ...)"`, `bash < <(curl ...)` and
  `bash <<< "$(curl ...)"`.
- **A file downloaded and then run in the same command**: `curl -o x.sh ... && chmod +x x.sh
  && ./x.sh`, `wget URL && sh install.sh`, `curl -s URL > i.sh; bash i.sh`. The file name
  comes from `-o`, `-O`, a redirect or the end of the URL.
- A shell reading stdin through a path (`bash /dev/stdin`, `sh /dev/fd/0`), a pipe into a
  subshell or group (`curl ... | (sh)`, `curl ... | { sh; }`), and a download kept in a
  variable and run from it in the same command (`x=$(curl ...); eval "$x"`,
  `sh -c "$x"`).
- All of these inside `sudo`, `bash -c '...'` and `ssh host '...'`.

The interpreters are `sh`, `bash`, `zsh`, `dash`, `ksh`, `python`, `perl`, `ruby`, `node`
and `source` (or `.`).

```text
ASK: this pipes a download into sh. The code runs before anyone has read it. Download it, read it, then run it, or confirm with the user.
```

The hook asks (`permissionDecision: ask`); the call waits for you and nothing is blocked
outright.

## What passes

- A download that is read, not run: `curl ... | jq .`, `curl ... | grep x`,
  `curl ... | tee out.txt`, `wget -qO- ... | tar xz`, `curl -o out.json ... && jq . out.json`.
- A pipe into a shell with a script of its own, where stdin is only data:
  `curl ... | sh -c 'wc -c'`, `curl ... | bash process.sh`, `curl ... | python3 -m json.tool`,
  `curl ... | python3 -c '...'`. A script that itself starts a shell on stdin
  (`curl ... | sh -c 'sh'`) still asks.
- Downloaded data passed as an argument: `python3 run.py "$(curl -s ...)"`, and a downloaded
  file that a script only reads: `curl -o d.csv ... && python3 process.py d.csv`.
- `bash -n x.sh` after a download: `-n` only checks the syntax.
- A local file piped in: `cat script.sh | bash`. That is not a download.
- Text that only mentions the pattern: `echo "curl x | sh"`, `grep -r "curl | sh" docs`, a
  commit message, a heredoc being written to a file.
- A command run inside a container: `docker run alpine sh -c 'wget -qO- ... | sh'` runs
  there, not on your machine.

If a command cannot be parsed, a `curl` or `wget` piped on to a shell still asks, read from
the text.

This is a speed bump, not a wall. These get past it:

- A script written earlier and run in a later call, a download by `git clone` or
  `pip install`, and a file name built from variables or run as
  `bash $PWD/x`, `sh -c 'bash x'` or a bare name from `PATH`.
- Other interpreters after the pipe: `fish`, `csh`, `tcsh`, `busybox sh`, `php`, `lua`,
  `deno run -`, `bun run -`, `Rscript -`, `pwsh`. And a download given to an interpreter's
  string option: `python3 -c "$(curl ...)"`, `perl -e`, `node -e`, `ruby -e`.
- Other fetchers: `http`, `fetch`, `aria2c`, `lynx -source`, `w3m -dump_source`, `nc`,
  `ssh host cat x`, and a Python `urllib` one-liner piped to a shell.
- A download saved with `tee` and run from that file (`curl ... | tee x.sh >/dev/null && bash
  x.sh`), and a variable that is passed on through a function or a second variable.
- PowerShell's `iex (iwr ...)`.

## Turning it off

You cannot. This guard has no family name in `disabled_guards`: with `pii`,
`raw-client-data`, `session-url`, `egress`, `git` and `sweep` all off, `curl ... | sh` still asks. You
can approve each ask when it appears. See [Options](options.md).
