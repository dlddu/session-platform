import { useEffect, useRef, useState } from "react";
import { Link, useParams } from "react-router-dom";
import { api } from "../api/client";
import type { Session } from "../api/types";
import { StateBadge } from "../app/StateBadge";

// Workspace — a single live session: the shell console plus the lifecycle
// side panel. The console is the J5 loop (docs/mockups/workspace.html):
// entering replays the full scrollback once (read offset=0), the `$` input
// row writes commands into the shell's stdin (AC-D2), and follow-up reads use
// the server-issued nextOffset cursor so only new output is appended (AC-D3).
// There is NO automatic polling: every read/write is a user action, so
// lastAccess only moves on real client shell I/O (AC-D5).
export function Workspace() {
  const { id = "" } = useParams();
  const [sess, setSess] = useState<Session | null>(null);
  const [error, setError] = useState<string | null>(null);
  const [term, setTerm] = useState("");
  const [cmd, setCmd] = useState("");
  // The read cursor lives in a ref: reads are sequential await chains, and the
  // cursor must advance immediately (not on the next render) to keep deltas
  // non-overlapping.
  const offsetRef = useRef(0);
  const termRef = useRef<HTMLDivElement>(null);

  useEffect(() => {
    offsetRef.current = 0;
    setTerm("");
    api.getSession(id).then(setSess).catch((e) => setError(String(e)));
    // Re-entering the workspace restores the entire session history (AC-D3
    // non-consuming: offset 0 always replays everything).
    readDelta().catch((e) => appendSys(`read failed: ${e}`));
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [id]);

  // Keep the scrollback pinned to the newest output.
  useEffect(() => {
    const el = termRef.current;
    if (el) el.scrollTop = el.scrollHeight;
  }, [term]);

  const appendSys = (line: string) =>
    setTerm((t) => (t === "" || t.endsWith("\n") ? t : t + "\n") + `◆ ${line}\n`);

  async function readDelta() {
    const r = await api.readSession(id, offsetRef.current);
    offsetRef.current = r.nextOffset;
    if (r.payload) setTerm((t) => t + r.payload);
    setSess(r.session);
  }

  const sleep = (ms: number) => new Promise((res) => setTimeout(res, ms));

  // One command round-trip: write cmd+"\n" into the shell's stdin, then a
  // short, bounded series of cursor reads to collect the echo and the command
  // output. Bounded (it always stops) — deliberately not a poller (AC-D5).
  // An empty Enter skips the write and just fetches the pending delta.
  async function run() {
    const command = cmd;
    setCmd("");
    try {
      if (command.trim() !== "") {
        await api.writeSession(id, command + "\n");
      }
      await readDelta();
      for (const ms of [250, 700]) {
        await sleep(ms);
        await readDelta();
      }
    } catch (e) {
      appendSys(`shell i/o failed: ${e}`);
    }
  }

  async function doSwitch() {
    try {
      const s = await api.switchSession(id);
      setSess(s);
      appendSys(`switch → ${s.state}`);
    } catch (e) {
      appendSys(`switch failed: ${e}`);
    }
  }

  if (error) return <div className="pad error">Failed to load session: {error}</div>;
  if (!sess) return <div className="pad empty">Loading…</div>;

  const frozen = sess.state === "snapshot";

  return (
    <div className="pad">
      <div className="crumbs">
        <Link to="/" className="back">
          ← Sessions
        </Link>
        <span>/</span>
        <span>{sess.name}</span>
      </div>

      <div className="h-top">
        <div>
          <h1>{sess.name}</h1>
          <div className="c-id">
            session/{sess.id} · {sess.pod ? `pod/${sess.pod}` : "pod reclaimed"}
          </div>
        </div>
        <StateBadge state={sess.state} />
      </div>

      <div className="ws-body">
        <div className="console">
          <div className="console-bar">
            <span>session shell</span>
            <span data-testid="ws-console-pod">
              {sess.pod ? `pod/${sess.pod}` : "pod reclaimed"}
            </span>
            {!frozen && (
              <span className="tag">
                <span className="led" />
                shell attached
              </span>
            )}
          </div>
          <div className="term" data-testid="ws-log" ref={termRef}>
            {term === "" ? (
              <span style={{ color: "var(--text-faint)" }}>
                {"// attached — the shell's output since session start appears here"}
              </span>
            ) : (
              term
            )}
          </div>
          <form
            className="input-row"
            onSubmit={(e) => {
              e.preventDefault();
              void run();
            }}
          >
            <span className="p">$</span>
            <input
              data-testid="ws-cmd"
              value={cmd}
              onChange={(e) => setCmd(e.target.value)}
              placeholder="run a command — executes in the session shell (bash)"
              spellCheck={false}
              autoComplete="off"
              aria-label="shell command"
            />
          </form>
        </div>

        <div>
          <div className={"lc-state" + (frozen ? " frozen" : "")}>
            <span className="big" data-testid="ws-state">{sess.state}</span>
          </div>
          <div className="panel">
            <h4>Actions</h4>
            <div style={{ display: "flex", flexDirection: "column", gap: 8 }}>
              <button className="btn btn-ghost" data-testid="ws-switch" onClick={doSwitch}>
                Switch (AC-C4)
              </button>
            </div>
          </div>
        </div>
      </div>
    </div>
  );
}
