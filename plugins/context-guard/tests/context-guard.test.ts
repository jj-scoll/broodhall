import { expect, mock, test } from 'claude-code/testing'

// Answers the mod's $.session.usage() with a context of `tokens`
function usageStub(tokens: number) {
  return () => ({ value: { context: { tokens, window: 1_000_000, percent: tokens / 10_000 } } })
}

// Records what reached Claude Code after the mod passed a prompt on
function promptSink(seen: { text?: string; context?: string[] }[]) {
  return ($, e) => {
    seen.push({ text: e.text, context: e.context })
    return { text: e.text }
  }
}

// ── Warn and block thresholds ────────────────────────────────────────────────

test('below the warning level a prompt goes through unchanged', async ($, on) => {
  const seen = []
  on('session.usage', usageStub(100_000))
  on('prompt.submit', promptSink(seen))
  on('ui.log', () => ({ value: undefined }))

  await $.prompt.submit({ text: 'hello' })

  expect(seen.length).toBe(1)
  expect(seen[0].text).toBe('hello')
  expect(seen[0].context ?? []).toEqual([])
})

test('between warn and block a prompt goes through with a note for Claude and a line for the user', async ($, on) => {
  const seen = []
  const logs: string[] = []
  on('session.usage', usageStub(200_000))
  on('prompt.submit', promptSink(seen))
  on('ui.log', ($, e) => {
    logs.push(e.text)
    return { value: undefined }
  })

  await $.prompt.submit({ text: 'keep going' })

  expect(seen.length).toBe(1)
  expect(seen[0].text).toBe('keep going')
  expect(seen[0].context.join(' ')).toMatch(/200k/)
  expect(seen[0].context.join(' ')).toMatch(/delegate wide searches/)
  expect(logs.join(' ')).toMatch(/200k/)
  // the guardrails gate and autoclear do the handoff and /clear on their own
  expect(seen[0].context.join(' ')).not.toMatch(/handoff|\/clear/)
  expect(logs.join(' ')).not.toMatch(/handoff|\/clear/)
})

test('at the block level a prompt is dropped with handoff and /clear advice', async ($, on) => {
  const seen = []
  on('session.usage', usageStub(260_000))
  on('prompt.submit', promptSink(seen))
  on('ui.log', () => ({ value: undefined }))

  const out = await $.prompt.submit({ text: 'one more thing' })

  expect(seen.length).toBe(0)
  expect(out.drop).toMatch(/260k/)
  expect(out.drop).toMatch(/\/memory:handoff/)
  expect(out.drop).toMatch(/\/clear/)
})

test('at the block level a prompt asking for a handoff still goes through', async ($, on) => {
  const seen = []
  on('session.usage', usageStub(320_000))
  on('prompt.submit', promptSink(seen))
  on('ui.log', () => ({ value: undefined }))

  await $.prompt.submit({ text: 'write a Handoff please' })

  expect(seen.length).toBe(1)
})

test('at the block level slash commands and shell escapes still go through', async ($, on) => {
  const seen = []
  on('session.usage', usageStub(400_000))
  on('prompt.submit', promptSink(seen))
  on('ui.log', () => ({ value: undefined }))

  await $.prompt.submit({ text: '/clear' })
  await $.prompt.submit({ text: '!git status' })

  expect(seen.length).toBe(2)
})

test('when usage cannot be read the prompt goes through', async ($, on) => {
  const seen = []
  on('session.usage', () => ({ deny: 'unavailable' }))
  on('prompt.submit', promptSink(seen))
  on('ui.log', () => ({ value: undefined }))

  await $.prompt.submit({ text: 'hello' })

  expect(seen.length).toBe(1)
})

test('the warn_tokens and block_tokens options move the thresholds', { options: { warn_tokens: 70_000, block_tokens: 100_000 } }, async ($, on) => {
  const seen = []
  let tokens = 60_000
  on('session.usage', () => usageStub(tokens)())
  on('prompt.submit', promptSink(seen))
  on('ui.log', () => ({ value: undefined }))

  await $.prompt.submit({ text: 'under' })
  tokens = 75_000
  await $.prompt.submit({ text: 'warned' })
  tokens = 120_000
  const out = await $.prompt.submit({ text: 'blocked' })

  expect(seen.map((p) => p.text)).toEqual(['under', 'warned'])
  expect(seen[0].context ?? []).toEqual([])
  expect(seen[1].context.join(' ')).toMatch(/75k/)
  expect(out.drop).toMatch(/limit 100k/)
})

test('options that are not positive numbers fall back to 180k and 260k', { options: { warn_tokens: 0, block_tokens: -1 } }, async ($, on) => {
  const seen = []
  on('session.usage', usageStub(200_000))
  on('prompt.submit', promptSink(seen))
  on('ui.log', () => ({ value: undefined }))

  await $.prompt.submit({ text: 'warned' })
  expect(seen.length).toBe(1)
  expect(seen[0].context.join(' ')).toMatch(/200k tokens; prompts are blocked at 260k/)
})

test('a block level at or below the warn level is lifted to the warn level', { options: { warn_tokens: 200_000, block_tokens: 150_000 } }, async ($, on) => {
  const seen = []
  on('session.usage', usageStub(190_000))
  on('prompt.submit', promptSink(seen))
  on('ui.log', () => ({ value: undefined }))

  await $.prompt.submit({ text: 'under both' })

  expect(seen.length).toBe(1)
})

// ── Clear after a handoff ────────────────────────────────────────────────────
const RESUME = 'Continue from the handoff in /w/HANDOFF.md.'
const BASE = '/data/guardrails'
const STATE = `${BASE}/claude-handoff`

// Wires a session at `tokens` whose state dir holds the files in `files` (a Set the test
// may change), whose launcher calls are recorded (`state` answers BASE, or exits 1 when
// `guardrails` is false; `autoclear --claim` answers RESUME, exit 1 once the .rearm file is
// gone or when `claim` is false), whose prompt box reads `box.text`, and whose /clear moves
// the session id from sess-1 to sess-2 (or rejects when `clearFails`)
function handoffWorld(on, opts: { tokens?: number; files?: string[]; claim?: boolean; clearFails?: boolean; guardrails?: boolean } = {}) {
  const seen = []
  const calls: (readonly string[])[] = []
  const argv: (readonly string[])[] = []
  const commands: string[] = []
  const fills: string[] = []
  const files = new Set(opts.files ?? [])
  const box = { text: '' }
  let id = 'sess-1'
  const clock = mock.clock(on)
  on('session.usage', usageStub(opts.tokens ?? 100_000))
  on('session.id', () => ({ value: id }))
  on('fs.exists', ($, e) => ({ value: files.has(e.path) }))
  on('process.run', ($, e) => {
    argv.push(e.argv)
    calls.push(e.argv.slice(2))
    if (e.argv[2] === 'state') {
      const ok = opts.guardrails !== false
      return { value: { exitCode: ok ? 0 : 1, stdout: ok ? `${BASE}\n` : '', stderr: '' } }
    }
    const rearm = `${STATE}/${e.argv[4]}.rearm`
    if (e.argv[3] === '--claim') {
      const ok = opts.claim !== false && files.delete(rearm)
      return { value: { exitCode: ok ? 0 : 1, stdout: ok ? `${RESUME}\n` : '', stderr: '' } }
    }
    return { value: { exitCode: 0, stdout: '', stderr: '' } }
  })
  on('command.run', ($, e) => {
    commands.push(e.command)
    if (opts.clearFails) throw new Error('no clear here')
    id = 'sess-2'
    return { value: {} }
  })
  on('prompt.read', () => ({ value: { text: box.text, cursor: box.text.length } }))
  on('prompt.fill', ($, e) => {
    fills.push(e.text)
    box.text = e.text
    return { isFilled: true }
  })
  on('prompt.submit', promptSink(seen))
  on('ui.log', () => ({ value: undefined }))
  on('turn.complete', ($, e) => ({ text: e.answer }))
  // calls to the launcher are recorded without its path, which is the plugin's own
  return { clock, seen, calls, argv, commands, fills, files, box }
}

const ARMED = [`${STATE}/sess-1.gate`, `${STATE}/sess-1.rearm`]
const mainTurnEnd = { answer: 'done', durationMs: 1, isAborted: false, turnId: 't1', reason: 'answer' }
const STATE_CALL = ['state']

test('once the handoff is written, an idle session clears and continues from it', async ($, on) => {
  const w = handoffWorld(on, { files: [`${STATE}/sess-1.gate`] })

  await $.turn.complete(mainTurnEnd)
  await w.clock.advance(30_000) // the writer is still running
  expect(w.commands).toEqual([])
  w.files.add(`${STATE}/sess-1.rearm`)
  await w.clock.advance(3_000)

  expect(w.commands).toEqual(['clear'])
  expect(w.seen.map((p) => p.text)).toEqual([RESUME])
  expect(w.calls).toEqual([
    STATE_CALL,
    ['autoclear', '--claim', 'sess-1'],
    ['autoclear', '--done', 'sess-1', 'ok', 'cleared by context-guard into sess-2'],
  ])
})

test('the launcher is the plugin\'s own scripts/run.sh, run by sh', async ($, on) => {
  const w = handoffWorld(on, { files: ARMED })

  await $.turn.complete(mainTurnEnd)
  await w.clock.advance(3_000)

  expect(w.argv.length).toBeGreaterThan(0)
  for (const a of w.argv) {
    expect(a[0]).toBe('sh')
    expect(a[1]).toMatch(/\/context-guard\/scripts\/run\.sh$/)
  }
})

test('a draft in the prompt box holds the clear until the box is empty', async ($, on) => {
  const w = handoffWorld(on, { files: ARMED })
  w.box.text = 'half a thou'

  await $.turn.complete(mainTurnEnd)
  await w.clock.advance(60_000)
  expect(w.commands).toEqual([])
  w.box.text = ''
  await w.clock.advance(3_000)

  expect(w.commands).toEqual(['clear'])
})

test('without a gate mark a finished turn starts no watch', async ($, on) => {
  const w = handoffWorld(on, { files: [`${STATE}/sess-1.rearm`] })

  await $.turn.complete(mainTurnEnd)
  await w.clock.advance(60_000)

  expect(w.calls.filter((c) => c[0] === 'autoclear')).toEqual([])
  expect(w.commands).toEqual([])
})

test("a subagent's turn end starts no watch", async ($, on) => {
  const w = handoffWorld(on, { files: ARMED })

  await $.turn.complete({ ...mainTurnEnd, agentId: 'a1' })
  await w.clock.advance(60_000)

  expect(w.commands).toEqual([])
})

test('a prompt typed once the handoff is written is carried into the cleared session', async ($, on) => {
  const w = handoffWorld(on, { files: ARMED })

  const out = await $.prompt.submit({ text: 'one more thing' })
  expect(out.drop).toMatch(/clear/)
  w.box.text = 'one more thing' // the host hands a dropped prompt back to the box
  await w.clock.advance(1_000)

  expect(w.commands).toEqual(['clear'])
  expect(w.seen.length).toBe(1)
  expect(w.seen[0].text.startsWith(RESUME)).toBe(true)
  expect(w.seen[0].text).toMatch(/one more thing$/)
  expect(w.box.text).toBe('')
})

test('a carried prompt leaves a newer draft in the box alone', async ($, on) => {
  const w = handoffWorld(on, { files: ARMED })

  await $.prompt.submit({ text: 'one more thing' })
  w.box.text = 'something else'
  await w.clock.advance(1_000)

  expect(w.fills).toEqual([])
  expect(w.box.text).toBe('something else')
})

test('a failed /clear is reported, re-armed by claude-tools, and not retried by the watch', async ($, on) => {
  const w = handoffWorld(on, { files: ARMED, clearFails: true })

  await $.turn.complete(mainTurnEnd)
  await w.clock.advance(3_000)
  w.files.add(`${STATE}/sess-1.rearm`) // what --done fail does
  await w.clock.advance(60_000)

  const autoclear = w.calls.filter((c) => c[0] === 'autoclear')
  expect(w.commands).toEqual(['clear'])
  expect(autoclear[1].slice(0, 4)).toEqual(['autoclear', '--done', 'sess-1', 'fail'])
  expect(autoclear[1][4]).toMatch(/^no clear: /)
  expect(w.seen).toEqual([])
})

test('at the block level with a re-armed handoff, the prompt is dropped, the session clears and the prompt is carried over', async ($, on) => {
  const w = handoffWorld(on, { tokens: 260_000, files: ARMED })

  const out = await $.prompt.submit({ text: 'one more thing' })
  expect(out.drop).toMatch(/clear/)
  await w.clock.advance(1_000)

  expect(w.commands).toEqual(['clear'])
  expect(w.seen.length).toBe(1)
  expect(w.seen[0].text.startsWith(RESUME)).toBe(true)
  expect(w.seen[0].text).toMatch(/one more thing$/)
})

test('at the block level with no re-armed handoff, the prompt is only dropped', async ($, on) => {
  const w = handoffWorld(on, { tokens: 260_000 })

  const out = await $.prompt.submit({ text: 'one more thing' })
  await w.clock.advance(1_000)

  expect(out.drop).toMatch(/\/memory:handoff/)
  expect(w.commands).toEqual([])
  expect(w.seen.length).toBe(0)
})

// ── Without the guardrails plugin ────────────────────────────────────────────

test('without a guardrails binary no clear is attempted, and the lookup is not repeated every turn', async ($, on) => {
  const w = handoffWorld(on, { guardrails: false, files: ARMED })

  await $.turn.complete(mainTurnEnd)
  await w.clock.advance(10_000)
  await $.turn.complete(mainTurnEnd)
  await w.clock.advance(10_000)

  expect(w.commands).toEqual([])
  expect(w.calls).toEqual([STATE_CALL])
})

test('the lookup is retried a minute later, so a guardrails install mid-session is picked up', async ($, on) => {
  const w = handoffWorld(on, { guardrails: false })

  await $.turn.complete(mainTurnEnd)
  await w.clock.advance(61_000)
  await $.turn.complete(mainTurnEnd)

  expect(w.calls).toEqual([STATE_CALL, STATE_CALL])
})

test('without guardrails the thresholds still work', async ($, on) => {
  const w = handoffWorld(on, { guardrails: false, tokens: 260_000 })

  const out = await $.prompt.submit({ text: 'one more thing' })

  expect(out.drop).toMatch(/\/memory:handoff/)
  expect(w.seen.length).toBe(0)
})
