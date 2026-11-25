- guardrails: the Bash guards read a command as the shell will run it, so respelling no longer
  slips past them. `\rm`, `r''m`, line continuations, `c=rm; "$c"`, `$'\x72\x6d'`,
  `"$(printf ...)"`, `{rm,-rf,~}`, `${IFS}`, a function the line defines, code piped or
  sourced into a shell (`printf ... | sh`, `base64 -d | sh`, `source <(...)`), the shell
  commands of `python -c` and `perl -e` one-liners, and paths such as `/home/../home/you` or
  `/hom[e]/you` all get the plain command's answer. A command name or piped code only known at
  run time asks (`dynamic-command`, `opaque-shell`). See [Bash core](../guardrails/bash-core.md). (#85)
- guardrails: `git push origin +main` blocks like `--force`; recursive `chmod` of a protected
  target blocks with any mode, not only `777`; `dd` blocks only when it writes to a device
  (`of=/dev/...`), so `dd if=/dev/zero of=disk.img` passes. (#85)
- guardrails: new asks, with no switch: `terraform`/`tofu`/`terragrunt destroy` and
  `apply -destroy` (`iac-destroy`, which replaces the plan note for a destroy),
  `aws s3 rm --recursive` and `s3 rb --force` (`s3-rm`), `find -delete` and `find -exec rm`
  (`find-delete`). `git push --delete` and `git push origin :branch` ask under the `git`
  family (`git-push-delete`). (#85)
