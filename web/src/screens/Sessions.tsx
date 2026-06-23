import { useEffect, useState } from "react";
import { Link, useNavigate } from "react-router-dom";
import { api } from "../api/client";
import type { Session } from "../api/types";
import { StateBadge } from "../app/StateBadge";

// Sessions console — lists every session from GET /api/v1/sessions and renders
// the summary strip + card grid. Cards route to Workspace (live) or Restore
// (snapshot). [plan step 5, O3]
export function Sessions() {
  const [sessions, setSessions] = useState<Session[] | null>(null);
  const [error, setError] = useState<string | null>(null);
  const navigate = useNavigate();

  useEffect(() => {
    api
      .listSessions()
      .then(setSessions)
      .catch((e) => setError(String(e)));
  }, []);

  const counts = {
    active: sessions?.filter((s) => s.state === "active").length ?? 0,
    idle: sessions?.filter((s) => s.state === "idle").length ?? 0,
    snapshot: sessions?.filter((s) => s.state === "snapshot").length ?? 0,
  };

  return (
    <div className="pad">
      <div className="h-top">
        <div>
          <div className="eyebrow">Control plane · us-east-1</div>
          <h1>Sessions</h1>
          <div className="sub">
            Every session runs in its own dedicated pod. Idle sessions freeze to
            a checkpoint and hand their compute back.
          </div>
        </div>
        <Link to="/new" className="btn btn-primary">
          + New session
        </Link>
      </div>

      <div className="summary">
        <div className="chip">
          <span className="dot" style={{ background: "var(--active)" }} />
          <b>{counts.active}</b>
          <span>active</span>
        </div>
        <div className="chip">
          <span className="dot" style={{ background: "var(--idle)" }} />
          <b>{counts.idle}</b>
          <span>idle</span>
        </div>
        <div className="chip">
          <span className="dot" style={{ background: "var(--frozen)" }} />
          <b>{counts.snapshot}</b>
          <span>frozen</span>
        </div>
      </div>

      {error && <div className="error">Failed to load sessions: {error}</div>}
      {!error && sessions === null && <div className="empty">Loading…</div>}
      {!error && sessions?.length === 0 && (
        <div className="empty">No sessions yet. Create one to get started.</div>
      )}

      <div className="grid">
        {sessions?.map((s) => (
          <button
            key={s.id}
            className="card"
            data-state={s.state}
            onClick={() =>
              navigate(s.state === "snapshot" ? `/restore/${s.id}` : `/session/${s.id}`)
            }
            aria-label={`${s.name}, ${s.state}`}
          >
            <div className="card-head">
              <div>
                <div className="c-name">{s.name}</div>
                <div className="c-id">session/{s.id}</div>
              </div>
              <StateBadge state={s.state} />
            </div>
            <div className="card-foot">
              <span>{s.pod ? `pod/${s.pod}` : "pod reclaimed"}</span>
              <span style={{ marginLeft: "auto" }}>{s.region}</span>
            </div>
          </button>
        ))}
      </div>
    </div>
  );
}
