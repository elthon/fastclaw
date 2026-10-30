// Some models (DeepSeek-backed gateways in particular) stream their tool
// calls as text — `<｜｜DSML｜｜ calls>`, `<｜tool▁calls｜>`,
// `<function_calls><invoke …>` — before the server recovers them as real
// tool calls. The server strips that markup from what it saves, but live
// deltas (and replies saved before the fix) still carry it, so the chat
// cuts the text at the first such tag.

// A leaked special-token tag: a DSML marker, or a pipe-delimited
// tool-call tag. Plain `<invoke>` / `<function_calls>` aren't matched, so
// a reply that explains that XML (in a code block, say) stays intact.
const LEAK_START = /<\s*\/?[\s|｜]*DSML|<\s*\/?\s*[|｜][\s|｜]*(?:tool[_▁]calls|function_calls|invoke)/;

// A partial tag at the very end of a stream (`<`, `<｜｜`, `<｜｜DS`) may
// be the start of one; hold it back until the next delta decides.
const PARTIAL_TAIL = /<[\s|｜/]*(?:D(?:S(?:M(?:L)?)?)?)?$/;

export function stripLeakedToolMarkup(text: string, streaming = false): string {
  const leak = text.search(LEAK_START);
  let out = leak >= 0 ? text.slice(0, leak) : text;
  if (streaming) out = out.replace(PARTIAL_TAIL, "");
  return leak >= 0 ? out.trimEnd() : out;
}
