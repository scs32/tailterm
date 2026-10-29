import { atom, read, update } from 'claude-code'
import type { Register, TurnUsage } from 'claude-code'

import type { Meter, TokenClasses } from '../types'

const ZERO: TokenClasses = { input: 0, output: 0, cacheRead: 0, cacheWrite: 0 }
const EMPTY: Meter = { turns: 0, withoutUsage: 0, main: ZERO, subagent: ZERO, last: null }
// Change the tag when Meter's fields change: it declines what an earlier
// version stored, which outlives a reload.
const meter = atom({ plugin: 'token-meter', key: 'meter' } as const, EMPTY, { shape: 'meter-1' })

const classesOf = (u: TurnUsage): TokenClasses => ({
  input: u.input_tokens,
  output: u.output_tokens,
  cacheRead: u.cache_read_input_tokens,
  cacheWrite: u.cache_creation_input_tokens,
})
const add = (a: TokenClasses, b: TokenClasses): TokenClasses => ({
  input: a.input + b.input,
  output: a.output + b.output,
  cacheRead: a.cacheRead + b.cacheRead,
  cacheWrite: a.cacheWrite + b.cacheWrite,
})
const total = (c: TokenClasses) => c.input + c.output + c.cacheRead + c.cacheWrite

// Units as `tt usage` prints them (hub/cmd/tt/usage.go formatTokenCount):
// plain below 1,000, then 12.1k, 6.71M, 1.23B; rounding up to 1000 moves to
// the next unit.
const UNITS = [{ size: 1e3, suffix: 'k', digits: 1 }, { size: 1e6, suffix: 'M', digits: 2 }, { size: 1e9, suffix: 'B', digits: 2 }]
const formatTokens = (n: number): string => {
  for (let i = UNITS.length - 1; i >= 0; i--) {
    let u = UNITS[i]
    if (Math.abs(n) < u.size) continue
    let text = (n / u.size).toFixed(u.digits)
    if (i < UNITS.length - 1 && text.replace(/^-/, '').startsWith('1000')) {
      u = UNITS[i + 1]
      text = (n / u.size).toFixed(u.digits)
    }
    return text + u.suffix
  }
  return String(n)
}
const turnsText = (m: Meter) =>
  `${m.turns} ${m.turns === 1 ? 'turn' : 'turns'}` + (m.withoutUsage > 0 ? ` (${m.withoutUsage} without usage)` : '')
const classesText = (c: TokenClasses) =>
  `${total(c)} (input ${c.input}, output ${c.output}, cache read ${c.cacheRead}, cache write ${c.cacheWrite})`

export const register: Register = on => {
  on('session.start', async ($, e, next) => {
    await $.command.register({
      name: 'token-meter',
      description: "Show this session's tokens by class, main loop and subagents apart",
    })

    return next(e)
  })

  // Observe only: counts the turn, sets this plugin's status line and passes
  // the turn on unchanged. A turn with no usage (an interrupt, an API error)
  // adds nothing and is counted apart, never as zero tokens.
  on('turn.complete', async ($, e, next) => {
    const used = e.usage ? classesOf(e.usage) : null
    const m = await update($, meter, was => ({
      turns: was.turns + 1,
      withoutUsage: was.withoutUsage + (used ? 0 : 1),
      main: used && e.agentId === undefined ? add(was.main, used) : was.main,
      subagent: used && e.agentId !== undefined ? add(was.subagent, used) : was.subagent,
      last: used,
    }))
    const item = await $.env.get('TAILTERM_WORK_ITEM')
    const turn = m.last ? formatTokens(total(m.last)) : 'unavailable'
    const session = formatTokens(total(m.main) + total(m.subagent))
    $.ui.status(`tokens: turn ${turn} · session ${session} over ${turnsText(m)}` + (item ? ` · ${item}` : ''))

    return next(e)
  })

  on('command.run', { command: 'token-meter' }, async $ => {
    const m = await read($, meter)
    const item = await $.env.get('TAILTERM_WORK_ITEM')
    const agent = await $.env.get('TAILTERM_AGENT_NAME')
    const task = await $.env.get('TAILTERM_TASK')
    const lines = [
      'Token meter: this session only, as the API reported it; not uploaded and not a ledger.',
      `Last turn: ${m.turns === 0 ? 'none yet' : m.last ? classesText(m.last) : 'unavailable (no usage reported)'}`,
      `Session: ${classesText(add(m.main, m.subagent))} over ${turnsText(m)}`,
      `  main loop: ${classesText(m.main)}`,
      `  subagents: ${classesText(m.subagent)}`,
    ]
    if (item) lines.push(`Item: ${item}` + (agent ? ` (agent ${agent})` : ''))
    lines.push(`Per item and phase, the hub's figures: tt usage --project ${task || 'TASK'} --item ${item || 'ITEM'}`)

    return { text: lines.join('\n') }
  })
}
