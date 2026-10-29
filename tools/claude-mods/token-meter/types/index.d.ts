// The four token classes the API reports for a turn, summed.
export type TokenClasses = {
  input: number
  output: number
  cacheRead: number
  cacheWrite: number
}

// This session's figures. Kept only in the session's $.state: never uploaded,
// written to a file or posted.
export type Meter = {
  turns: number
  withoutUsage: number
  main: TokenClasses
  subagent: TokenClasses
  // The last turn's classes, or null when it ended with no usage.
  last: TokenClasses | null
}

declare module 'claude-code' {
  interface PluginState {
    'token-meter': { meter: Shaped<Meter> }
  }
}
