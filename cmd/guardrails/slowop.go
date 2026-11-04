package main

// Plan-before-apply nudge, replacing the SLOW_OP warning. SLOW_OP exited 1, which Claude
// Code treats as a non-blocking error: Claude sees at most a one-line notice (SLOW_OP shows
// up once in tool_calls.error_text and 0 times in hook_context across ~5,390 matching
// calls, 158 of them terraform/terragrunt apply|destroy). The nudge now exits 0 with
// hookSpecificOutput.additionalContext, which PreToolUse delivers to the model next to the
// tool result, and only where it earns its place:
//
//   - terraform|terragrunt apply|destroy when this session has run no `plan` for the same
//     directory in the last 30 minutes (applying a saved plan file is exempt);
//   - docker build, once per directory per 30 minutes.
//
// pytest, go build, cargo build, npm run build|test and yarn build no longer warn: the TDD
// rule requires running tests, so a warning against them contradicted it.

import (
	"path"
	"path/filepath"
	"strings"
	"time"
)

const slowWindow = 30 * time.Minute

func (e *guardEnv) seen(key string) bool {
	return e.State != nil && e.State.Seen(key, slowWindow, e.Now)
}

func (e *guardEnv) mark(key string) {
	if e.State != nil {
		e.State.Mark(key, e.Now)
	}
}

func resolveDir(dir, p, home string) string {
	switch {
	case p == "" || strings.HasPrefix(p, "-"):
		return dir
	case p == "~" || strings.HasPrefix(p, "~/"):
		p = filepath.Join(home, strings.TrimPrefix(p, "~"))
	}
	if !filepath.IsAbs(p) {
		p = filepath.Join(dir, p)
	}
	return filepath.Clean(p)
}

var dirFlags = []string{"-chdir", "--terragrunt-working-dir", "--working-dir", "--tf-path-dir"}

// tfArgs finds the terraform/terragrunt subcommand (plan, apply, destroy or other), its
// positional arguments and the working directory the command targets.
func tfArgs(args []string, dir, home string) (sub string, positional []string, wdir string) {
	wdir = dir
	for i := 0; i < len(args); i++ {
		a := args[i]
		flagged := false
		for _, f := range dirFlags {
			if strings.HasPrefix(a, f+"=") {
				wdir, flagged = resolveDir(dir, strings.TrimPrefix(a, f+"="), home), true
			} else if a == f && i+1 < len(args) && f != "-chdir" {
				wdir, flagged = resolveDir(dir, args[i+1], home), true
				i++
			}
			if flagged {
				break
			}
		}
		switch {
		case flagged || a == "--":
		case strings.HasPrefix(a, "-"):
		case sub == "" && (a == "run-all" || a == "run"):
		case sub == "":
			sub = a
		default:
			positional = append(positional, a)
		}
	}
	return sub, positional, wdir
}

func slowOpRule(env *guardEnv, cmd string) decision {
	segs, ok := shellSegments(stripHeredocs(cmd))
	if !ok {
		return decision{} // a nudge, not a block: stay quiet on input we cannot parse
	}
	dir := env.Cwd
	var out decision
	for _, seg := range segs {
		seg = peelWrappers(seg)
		if len(seg) == 0 {
			continue
		}
		args := seg[1:]
		switch path.Base(seg[0]) {
		case "cd":
			if len(args) > 0 {
				dir = resolveDir(dir, args[0], env.Home)
			}
		case "terraform", "terragrunt":
			sub, pos, wdir := tfArgs(args, dir, env.Home)
			switch sub {
			case "plan":
				env.mark("plan:" + wdir)
			case "apply", "destroy":
				planFile := false
				for _, p := range pos {
					planFile = planFile || strings.Contains(strings.ToLower(p), "plan")
				}
				if out.Context == "" && !planFile && !env.seen("plan:"+wdir) {
					out = decision{Rule: "plan-before-apply", Context: "You are about to run `" + path.Base(seg[0]) + " " + sub +
						"` in " + wdir + " and this session has run no `plan` there in the last 30 minutes. Run `" + path.Base(seg[0]) +
						" plan` first and check the diff, or apply a saved plan file; if a plan is genuinely unnecessary, say why."}
				}
			}
		case "docker":
			if len(args) > 0 && args[0] == "build" || len(args) > 1 && args[0] == "buildx" && args[1] == "build" {
				if out.Context == "" && !env.seen("dockerbuild:"+dir) {
					out = decision{Rule: "slow-op", Context: "SLOW_OP: `docker build` detected. Only run this if there is a concrete reason (e.g. verifying an image). If this is exploratory, use a cheaper check first."}
				}
				env.mark("dockerbuild:" + dir)
			}
		}
	}
	return out
}
