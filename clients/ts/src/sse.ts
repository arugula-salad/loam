// Server-Sent Events: the ports watch and the event stream.

export interface SSEMessage {
  event: string;
  data: string;
}

/**
 * readSSE yields the events of an SSE body: each one's name ("message" when
 * it has none) and its data lines joined. Comments (keep-alives) are skipped.
 */
export async function* readSSE(body: ReadableStream<Uint8Array>): AsyncGenerator<SSEMessage> {
  const decoder = new TextDecoder();
  const reader = body.getReader();
  let buf = "";
  let event = "";
  let data: string[] | null = null;
  try {
    for (;;) {
      const { value, done } = await reader.read();
      buf += done ? decoder.decode() : decoder.decode(value, { stream: true });
      for (;;) {
        const nl = buf.search(/\r\n|\r|\n/);
        if (nl < 0) break;
        // A lone \r at the end of what has arrived may be the first half of \r\n.
        if (buf[nl] === "\r" && nl === buf.length - 1 && !done) break;
        const line = buf.slice(0, nl);
        buf = buf.slice(nl + (buf.startsWith("\r\n", nl) ? 2 : 1));
        if (line === "") {
          if (data !== null || event !== "") {
            yield { event: event || "message", data: (data ?? []).join("\n") };
          }
          event = "";
          data = null;
          continue;
        }
        if (line.startsWith(":")) continue;
        const colon = line.indexOf(":");
        const field = colon < 0 ? line : line.slice(0, colon);
        let value = colon < 0 ? "" : line.slice(colon + 1);
        if (value.startsWith(" ")) value = value.slice(1);
        if (field === "event") event = value;
        else if (field === "data") (data ??= []).push(value);
      }
      if (done) return;
    }
  } finally {
    await reader.cancel().catch(() => {});
  }
}
