// A captured history snapshot may style text, but cannot control the viewer,
// request clipboard contents, set titles, switch buffers, or emit replies.
export function historyText(value) {
  const text = String(value)
    .replace(
      /(?:\x1b[\]PX^_]|[\x90\x98\x9d-\x9f])[\s\S]*?(?:\x07|\x1b\\|\x9c|$)/g,
      "",
    )
    .replace(
      /(?:\x1b\[|\x9b)([0-?]*)([ -/]*)([@-~])/g,
      (_, parameters, intermediate, final) =>
        final === "m" && !intermediate && /^[0-9;:]*$/.test(parameters)
          ? `\x1b[${parameters}m`
          : "",
    );
  return Array.from(
    text.matchAll(/\x1b\[[0-9;:]*m|[^\x00-\x08\x0b-\x1f\x7f-\x9f]+/g),
    (m) => m[0],
  ).join("");
}
