const TURN_SENTINEL = "\u001e";
const TURN_LEAD = "session-platform ";

export function renderAgentTranscript(raw: string): string {
  if (!raw.includes(TURN_SENTINEL)) return raw;
  const parts = raw.split(TURN_SENTINEL);
  let shown = parts[0];
  for (const part of parts.slice(1)) {
    const newline = part.indexOf("\n");
    if (newline < 0) continue;
    const header = part.slice(0, newline);
    const body = part.slice(newline + 1);
    const kind = header.startsWith(TURN_LEAD)
      ? header.slice(TURN_LEAD.length).split(" ")[0]
      : "";
    if (kind === "input") {
      shown += "▸ " + body.replace(/\n$/, "").split("\n").join("\n  ") + "\n";
      continue;
    }
    if (kind === "end") {
      const status = /(?:^| )status=(\S+)/.exec(header)?.[1];
      if (status && status !== "ok") shown += `◆ turn ended: ${status}\n`;
    }
    shown += body;
  }
  return shown;
}
