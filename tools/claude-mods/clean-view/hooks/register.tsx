import { atom, read, update } from 'claude-code'
import type { Register } from 'claude-code'

const isOff = atom({ plugin: 'clean-view', key: 'isOff' } as const, false)

export const register: Register = on => {
  on('session.start', async ($, e, next) => {
    await $.command.register({
      name: 'clean-view',
      description: 'Toggle hiding tool calls in the transcript',
    })

    return next(e)
  })

  on('command.run', { command: 'clean-view' }, async $ => {
    const off = await update($, isOff, v => !(v ?? false))

    return { text: off ? 'Clean view off: tool calls are shown.' : 'Clean view on: tool calls are hidden.' }
  })

  for (const component of ['ToolUse', 'ToolResult', 'ToolGroup', 'ToolProgress'] as const) {
    on('ui.render', { component }, async ($, e, next) => {
      if (await read($, isOff)) {
        return next(e)
      }

      const { Box } = $.ui.resolve(e)

      return <Box />
    })
  }
}
