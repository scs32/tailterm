export type CleanViewOff = boolean

declare module 'claude-code' {
  interface PluginState {
    'clean-view': { isOff: CleanViewOff }
  }
}
