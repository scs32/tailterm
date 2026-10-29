import type { Hook, Register } from 'claude-code'

// The secret redaction mod (docs/claude-mods-secret-redaction.md). It covers
// the rows a settings hook cannot rewrite: a typed or pasted prompt, a prompt
// queued while a turn runs, an @file attachment and the output of a failed
// tool call. For each it hands the row's text to `tt hook redact` on standard
// input. tt decides the scope, reads the host setting and holds the only
// pattern definition; the mod keeps none of them, and writes nothing.
//
// Every failure keeps the row as the engine made it: no tt, a slow, failed or
// silent one, or an answer that is not exactly the expected one.

type Engine = Parameters<Hook<'session.append'>>[0]
type Row = Parameters<Hook<'session.append'>>[1]
type Block = Row['message']['content'][number]

const TT_TIMEOUT_MS = 1000

// The covered rows, each seen in the design's sandbox probes.
function isCovered(e: Row): boolean {
  switch (e.door) {
    case 'prompt':
    case 'queue':
    case 'tool-result':
      return true
    case 'delivery':
      return e.message.name === 'queued_command'
    case 'attachment':
      return e.origin.kind === 'engine' && e.message.name === 'file'
    default:
      return false
  }
}

// A slot is one string tt is asked about and where its answer goes back.
type Slot = { text: string; put: (text: string) => Block }

// The slots of one block. On the tool-result door only a failed tool_result
// is covered (the settings hook rewrites successful output); elsewhere every
// text block is. Media and every other kind are never touched.
function blockSlots(door: Row['door'], block: Block, index: number, blocks: Block[]): Slot[] {
  if (door !== 'tool-result') {
    if (block.type !== 'text' || typeof block.text !== 'string') return []
    return [{ text: block.text, put: text => (blocks[index] = { ...blocks[index], text }) }]
  }
  if (block.type !== 'tool_result' || block.is_error !== true) return []
  const content = block.content
  if (typeof content === 'string') {
    return [{ text: content, put: text => (blocks[index] = { ...blocks[index], content: text }) }]
  }
  if (!Array.isArray(content)) return []
  const inner = content.map(part => ({ ...(part as Block) }))
  const slots: Slot[] = []
  inner.forEach((part, at) => {
    if (part.type !== 'text' || typeof part.text !== 'string') return
    slots.push({
      text: part.text,
      put: text => {
        inner[at] = { ...inner[at], text }
        return (blocks[index] = { ...blocks[index], content: inner })
      },
    })
  })
  return slots
}

// The answer's texts when it is exactly {"v":1,"texts":[...]} with one string
// per slot; otherwise undefined.
function answerTexts(stdout: string, count: number): string[] | undefined {
  let answer: unknown
  try {
    answer = JSON.parse(stdout)
  } catch {
    return undefined
  }
  if (typeof answer !== 'object' || answer === null || Array.isArray(answer)) return undefined
  const { v, texts } = answer as { v?: unknown; texts?: unknown }
  if (v !== 1 || !Array.isArray(texts) || texts.length !== count) return undefined
  return texts.every(text => typeof text === 'string') ? (texts as string[]) : undefined
}

// The row with tt's texts put back, or undefined to keep it as made.
async function redactRow($: Engine, e: Row): Promise<Row | undefined> {
  const blocks = [...e.message.content]
  const slots = blocks.flatMap((block, index) => blockSlots(e.door, block, index, blocks))
  if (slots.length === 0) return undefined
  const tool = e.origin.kind === 'tool' ? e.origin.tool : ''
  const stdin = JSON.stringify({ v: 1, door: e.door, tool, texts: slots.map(slot => slot.text) })
  // The text goes on standard input only: argv and the environment are
  // visible to other processes. The child does not inherit the session id,
  // which tt needs to find the owner helper registered from this session.
  const ran = await $.process.run(['tt', 'hook', 'redact'], {
    stdin,
    env: { CLAUDE_CODE_SESSION_ID: await $.session.id() },
    timeoutMs: TT_TIMEOUT_MS,
  })
  if (ran.exitCode !== 0 || ran.stdout === '') return undefined
  const texts = answerTexts(ran.stdout, slots.length)
  if (texts === undefined) return undefined
  slots.forEach((slot, at) => slot.put(texts[at]))
  return { ...e, message: { ...e.message, content: blocks } }
}

export const register: Register = on => {
  on('session.append', async ($, e, next) => {
    if (!isCovered(e)) return next(e)
    let rewritten: Row | undefined
    try {
      rewritten = await redactRow($, e)
    } catch {
      rewritten = undefined
    }
    return next(rewritten ?? e)
  }).catch(($, e, next) => next(e))
}
