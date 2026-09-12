// Keep in sync with api.MaxAgentWorkItemContextBytes. Bounds apply to UTF-8
// serialized JSON, including every source/revision/message; never truncate.
export const MAX_WORK_CONTEXT_BYTES = 256 * 1024;
const encoder = new TextEncoder();

export function serializedWorkContext(value) {
  const serialized = typeof value === "string" ? value : JSON.stringify(value);
  if (
    !serialized ||
    encoder.encode(serialized).byteLength > MAX_WORK_CONTEXT_BYTES
  )
    throw new Error(
      "This item’s complete immutable history exceeds the 256 KiB session-context limit. Starting this team is currently unsupported; no context was truncated.",
    );
  if (new TextDecoder().decode(encoder.encode(serialized)) !== serialized)
    throw new Error("Prepared work-item context must be valid UTF-8 JSON.");
  JSON.parse(serialized);
  return serialized;
}

// Match encoding/json's RawMessage compaction without parsing/re-encoding its
// numbers, escape spelling, key order or string contents. Existing frozen JSON
// can contain integers beyond JavaScript's exact numeric range.
export function compactWorkContext(value) {
  const serialized = serializedWorkContext(value);
  let quoted = false,
    escaped = false,
    result = "";
  for (const character of serialized) {
    if (quoted) {
      result += character;
      if (escaped) escaped = false;
      else if (character === "\\") escaped = true;
      else if (character === '"') quoted = false;
    } else if (character === '"') {
      quoted = true;
      result += character;
    } else if (!/[\t\r\n ]/.test(character)) result += character;
  }
  return result;
}
