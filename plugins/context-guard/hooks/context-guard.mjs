// Context guard: runs the /clear at the end of the guardrails handoff chain, and keeps a
// session's context from growing without limit.
//
// Clear after a handoff: the guardrails plugin's `autoclear` Stop hook has the handoff
// written in the background and arms the clear (<state>/<session>.rearm). Once a main-loop
// turn ends with the gate mark there, a watch checks every CLEAR_POLL_MS; when the clear is
// armed and the prompt box is empty, it takes it (`autoclear --claim`), runs /clear and
// submits the continue prompt in the fresh session. A prompt typed while it is armed is
// dropped and carried into the fresh session instead. `autoclear --done` logs each outcome
// to writer.log and re-arms a failed clear for the next prompt.
//
// prompt.submit: reads the context size from $.session.usage(). At the warn level it logs a
// line for the user and adds a note for Claude; at the block level it drops the prompt,
// except handoff requests, slash commands and shell escapes, so /clear and /memory:handoff
// always work. The levels are the plugin's warn_tokens and block_tokens options.
//
// The binary and the state dir come from scripts/run.sh (this plugin's launcher), which finds
// what the guardrails plugin installed. Without guardrails the clear is skipped; the warn and
// block levels still work.
//
// The host reads on(...) and $.noun.method(...) from source, so they are spelled literally,
// and helpers that take $ are top-level functions.

const DEFAULT_WARN = 180_000
const DEFAULT_BLOCK = 260_000
const CLEAR_POLL_MS = 3_000
// The writer takes ~1.5 min; a watch that long without an armed clear stops.
const CLEAR_WATCH_MS = 2 * 60 * 60_000
// A failed lookup of guardrails' state dir is not repeated more often than this.
const LOOKUP_RETRY_MS = 60_000

function positive(value, fallback) {
  return typeof value === 'number' && Number.isFinite(value) && value > 0 ? value : fallback
}

export function register(on, options) {
  const WARN = positive(options?.warn_tokens, DEFAULT_WARN)
  // A block level under the warn level would block before it warns.
  const BLOCK = Math.max(positive(options?.block_tokens, DEFAULT_BLOCK), WARN)
  let watch = null
  const cache = { base: null, lookedUpAt: -Infinity }

  on('turn.complete', async ($, e, next) => {
    const result = await next(e)
    if (e.agentId) return result
    if (!watch && (await handoffFile($, cache, 'gate'))) {
      const started = await $.clock.now()
      watch = $.clock.every(CLEAR_POLL_MS, async () => {
        if ((await $.clock.now()) - started > CLEAR_WATCH_MS) return stopWatch()
        if (!(await handoffFile($, cache, 'rearm'))) return
        if ((await $.prompt.read()).text.trim()) return
        stopWatch()
        const resume = await claimClear($)
        if (resume) await clearInto($, resume)
      })
    }
    return result
  })

  on('prompt.submit', async ($, e, next) => {
    const text = e.text ?? ''
    if (/^\s*[\/!]/.test(text)) return next(e)
    // The handoff is written and the clear armed: clear now, and carry this prompt
    // into the fresh session.
    const resume = (await handoffFile($, cache, 'rearm')) && (await claimClear($))
    if (resume) {
      stopWatch()
      $.clock.after(0, () => clearInto($, resume, text))
      return { drop: 'The handoff is written: clearing into a fresh session and carrying this prompt over.' }
    }
    const tokens = await contextTokens($)
    if (tokens === null || tokens < WARN) return next(e)
    if (tokens >= BLOCK && !/handoff/i.test(text)) {
      return {
        drop: `Context is ${short(tokens)} tokens (limit ${short(BLOCK)}). Run /memory:handoff, then /clear, and continue in the fresh context. Prompts mentioning "handoff" still go through.`,
      }
    }
    // No handoff advice here: the guardrails gate fires first (at 80% of the auto-compact
    // window) and its hooks write the handoff and /clear on their own. The drop above stays
    // as the fallback.
    $.ui.log(`context ${short(tokens)} of ${short(BLOCK)} limit`)
    const note = `Context is ${short(tokens)} tokens; prompts are blocked at ${short(BLOCK)}. Keep tool output small and delegate wide searches to subagents.`
    return next({ ...e, context: [...(e.context ?? []), note] })
  })

  function stopWatch() {
    watch?.cancel()
    watch = null
  }
}

// Guardrails' state base dir, from the launcher; cached in `cache` once found, and a failed
// lookup is not repeated within LOOKUP_RETRY_MS.
async function findState($, cache) {
  if (cache.base) return cache.base
  try {
    const now = await $.clock.now()
    if (now - cache.lookedUpAt < LOOKUP_RETRY_MS) return null
    cache.lookedUpAt = now
    const run = await launcher($, ['state'])
    if (run.exitCode === 0 && run.stdout.trim()) cache.base = run.stdout.trim()
  } catch {
    // no launcher answer: no clear
  }
  return cache.base
}

// Whether <state>/<session>.<kind> exists: the gate mark ('gate') or an armed clear
// ('rearm') in guardrails' handoff state dir.
async function handoffFile($, cache, kind) {
  try {
    const base = await findState($, cache)
    if (!base) return false
    return await $.fs.exists(`${base}/claude-handoff/${await $.session.id()}.${kind}`)
  } catch {
    return false
  }
}

async function contextTokens($) {
  try {
    const { context } = await $.session.usage()
    return typeof context?.tokens === 'number' ? context.tokens : null
  } catch {
    return null
  }
}

// Runs this plugin's launcher, scripts/run.sh, with `args`.
async function launcher($, args) {
  return $.process.run(['sh', `${$.plugin.root}/scripts/run.sh`, ...args])
}

// The continue prompt of the clear `guardrails autoclear` armed for this session
// (`autoclear --claim` takes it, once), else null.
async function claimClear($) {
  try {
    const run = await launcher($, ['autoclear', '--claim', await $.session.id()])
    return run.exitCode === 0 && run.stdout.trim() ? run.stdout.trim() : null
  } catch {
    return null
  }
}

// Runs /clear and submits `resume` in the fresh session, with the prompt `carried` (one
// the person typed, dropped) appended; reports the outcome to `autoclear --done`, which
// re-arms a failed clear.
async function clearInto($, resume, carried) {
  const old = await $.session.id()
  try {
    await $.command.run({ command: 'clear' })
    const fresh = await $.session.id()
    if (fresh === old) throw new Error('session id unchanged after /clear')
    // The host hands a dropped prompt back to the box; it is carried, so empty it.
    if (carried && (await $.prompt.read()).text.trim() === carried.trim()) {
      await $.prompt.fill({ text: '', mode: 'replace' })
    }
    const text = carried ? `${resume}\n\nThen answer the prompt I typed just before the clear:\n\n${carried}` : resume
    await $.prompt.submit({ text, asUser: true })
    await launcher($, ['autoclear', '--done', old, 'ok', `cleared by context-guard into ${fresh}`])
  } catch (err) {
    await launcher($, ['autoclear', '--done', old, 'fail', `no clear: ${err?.message ?? err}`]).catch(() => {})
  }
}

function short(n) {
  if (n >= 1_000_000) return `${(n / 1_000_000).toFixed(1).replace(/\.0$/, '')}M`
  if (n >= 1_000) return `${Math.round(n / 1_000)}k`
  return String(n)
}
