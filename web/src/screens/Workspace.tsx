import { useEffect, useState } from "react";
import { Link, useParams } from "react-router-dom";
import { api } from "../api/client";
import type { Session } from "../api/types";
import { StateBadge } from "../app/StateBadge";

// Workspace — a single live session: console (panel) + lifecycle side panel
// with read/write/switch actions that exercise the state-branched stub
// endpoints. [plan step 5]
export function Workspace() {
  const { id = "" } = useParams();
  const [sess, setSess] = useState<Session | null>(null);
  const [error, setError] = useState<string | null>(null);
  const [log, setLog] = useState<string[]>([]);

  const refresh = () =>
    api.getSession(id).then(setSess).catch((e) => setError(String(e)));

  useEffect(() => {
    refresh();
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [id]);

  const append = (line: string) => setLog((l) => [...l, line]);

  async function doRead() {
    try {
      const r = await api.readSession(id);
      append(`read  → ${r.path}  (${r.payload})`);
      setSess(r.session);
    } catch (e) {
      append(`read  ✗ ${e}`);
    }
  }
  async function doWrite() {
    try {
      const r = await api.writeSession(id, "stub-write");
      append(`write → ${r.path}`);
      setSess(r.session);
    } catch (e) {
      append(`write ✗ ${e}`);
    }
  }
  async function doSwitch() {
    try {
      const s = await api.switchSession(id);
      append(`switch → ${s.state}`);
      setSess(s);
    } catch (e) {
      append(`switch ✗ ${e}`);
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
            <span>console · {sess.id}</span>
          </div>
          <div className="term" data-testid="ws-log">
            {log.length === 0 && (
              <div style={{ color: "var(--text-faint)" }}>
                // run a read / write / switch to exercise the stub endpoints
              </div>
            )}
            {log.map((l, i) => (
              <div key={i}>{l}</div>
            ))}
          </div>
        </div>

        <div>
          <div className={"lc-state" + (frozen ? " frozen" : "")}>
            <span className="big" data-testid="ws-state">{sess.state}</span>
          </div>
          <div className="panel">
            <h4>Actions</h4>
            <div style={{ display: "flex", flexDirection: "column", gap: 8 }}>
              <button className="btn btn-ghost" data-testid="ws-read" onClick={doRead}>
                Read (AC-C2)
              </button>
              <button className="btn btn-ghost" data-testid="ws-write" onClick={doWrite}>
                Write (AC-C3)
              </button>
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
