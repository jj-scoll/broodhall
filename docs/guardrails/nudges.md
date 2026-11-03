# Nudges

<span class="badge badge-note">NOTE</span> before a `terraform apply` with no plan, and before a `docker build`.

!!! why "Why it exists"
    Applying infrastructure changes you have not planned, and building an image to see
    whether something works, are the kind of step that is easy to take on reflex and costly to
    take for nothing. A refusal would be too much, since both are sometimes exactly right, so
    the guard lets the call run and puts a short reminder in front of Claude, which then
    plans first or says why it need not.

## What it catches

Nothing is blocked. The call runs, and a note reaches Claude next to the tool result.

### Apply or destroy with no plan

`terraform` or `terragrunt` with `apply` or `destroy`, when this session has run no `plan`
for the same directory in the last 30 minutes. The directory is the one the command targets:
the working directory, a `cd` earlier in the command, or `-chdir=`,
`--terragrunt-working-dir` or `--working-dir`.

```text
You are about to run `terraform apply` in /home/you/infra and this session has run no `plan` there in the last 30 minutes. Run `terraform plan` first and check the diff, or apply a saved plan file; if a plan is genuinely unnecessary, say why.
```

`terragrunt destroy` gets the same note with `terragrunt destroy` and `terragrunt plan` in it.

### docker build

`docker build` and `docker buildx build`, once per directory per 30 minutes.

```text
SLOW_OP: `docker build` detected. Only run this if there is a concrete reason (e.g. verifying an image). If this is exploratory, use a cheaper check first.
```

## What passes

- An apply or destroy after a `plan` in the same directory in the same session, within 30
  minutes: `terraform plan && terraform apply`, or `terraform -chdir=infra plan` then
  `terraform -chdir=infra apply`. A plan in a different directory does not count.
- Applying a saved plan file: any argument with `plan` in its name, such as
  `terraform apply plan.tfplan`.
- A second `docker build` in the same directory inside 30 minutes. A build in another
  directory gets its own note.
- Every other command. Builds and tests (`go test`, `pytest`, `npm run build`) do not warn,
  because running tests is the point; `docker ps` and the like pass.
- A command the guard cannot parse stays quiet: this is a reminder, not a block.

The record of plans and builds is kept per session, in the plugin's state directory.

## Turning it off

You cannot. Nudges have no family name in `disabled_guards`, and none of the five families
(`pii`, `raw-client-data`, `session-url`, `egress`, `git`) controls them. They never block, so
the cost of leaving them on is one short note. You can disable the plugin. See
[Options](options.md).
