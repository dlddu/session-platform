import { useCallback, useEffect, useState } from "react";
import { Link } from "react-router-dom";
import { api } from "../api/client";
import type { Session } from "../api/types";
import { SessionCard } from "../app/SessionCard";
import { PodIcon } from "../app/icons";
import { useToast } from "../app/Toast";

// Sums the per-checkpoint "N vCPU · M GB" reclaimed strings into one figure for
// the summary strip's "reclaimed from frozen" chip. Returns null when no
// snapshot exposes a parseable reclaimed value (chip is then hidden).
function aggregateReclaimed(sessions: Session[]): string | null {
  let vcpu = 0;
  let gb = 0;
  let any = false;
  for (const s of sessions) {
    const r = s.checkpoint?.reclaimed;
    if (!r) continue;
    const cpu = r.match(/([\d.]+)\s*vCPU/i);
    const mem = r.match(/([\d.]+)\s*GB/i);
    if (cpu) {
      vcpu += parseFloat(cpu[1]);
      any = true;
    }
    if (mem) {
      gb += parseFloat(mem[1]);
      any = true;
    }
  }
  if (!any) return null;
  const fmt = (n: number) => (Number.isInteger(n) ? String(n) : n.toFixed(1));
  return `${fmt(vcpu)} vCPU · ${fmt(gb)} GB`;
}

// Sessions console — lists every session from GET /api/v1/sessions and renders
// the summary strip + card grid. Cards route to Workspace (live) or Restore
// (snapshot). [plan step 5, O3]
export function Sessions() {
  const [sessions, setSessions] = useState<Session[] | null>(null);
  const [error, setError] = useState<string | null>(null);
  const [now, setNow] = useState(() => Date.now());
  const { toast } = useToast();

  const load = useCallback(() => {
    return api
      .listSessions()
      .then((list) => {
        setSessions(list);
        setError(null);
      })
      .catch((e) => setError(String(e)));
  }, []);

  useEffect(() => {
    load();
  }, [load]);

  // Live clock — drives the idle freeze countdown / snapshot "frozen ago"
  // without refetching. Only ticks while an idle card is on screen.
  const hasIdle = sessions?.some((s) => s.state === "idle") ?? false;
  useEffect(() => {
    if (!hasIdle) return;
    const t = setInterval(() => setNow(Date.now()), 1000);
    return () => clearInterval(t);
  }, [hasIdle]);

  const refresh = useCallback(() => {
    setNow(Date.now());
    load().then(() => toast("Sessions refreshed"));
  }, [load, toast]);

  const counts = {
    active: sessions?.filter((s) => s.state === "active").length ?? 0,
    idle: sessions?.filter((s) => s.state === "idle").length ?? 0,
    snapshot: sessions?.filter((s) => s.state === "snapshot").length ?? 0,
  };
  const reclaimed = sessions ? aggregateReclaimed(sessions) : null;

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
        <div style={{ display: "flex", gap: 10 }}>
          <button
            className="btn btn-ghost"
            onClick={refresh}
            data-testid="refresh-sessions"
          >
            <svg viewBox="0 0 24 24" fill="none" stroke="currentColor" strokeWidth="2" strokeLinecap="round" strokeLinejoin="round">
              <path d="M21 12a9 9 0 1 1-2.6-6.4M21 3v6h-6" />
            </svg>
            Refresh
          </button>
          <Link to="/new" className="btn btn-primary" data-testid="new-session-link">
            <svg viewBox="0 0 24 24" fill="none" stroke="currentColor" strokeWidth="2.3" strokeLinecap="round">
              <path d="M12 5v14M5 12h14" />
            </svg>
            New session
          </Link>
        </div>
      </div>

      <div className="summary">
        <div className="chip">
          <span className="dot" style={{ background: "var(--active)", boxShadow: "0 0 8px var(--active)" }} />
          <b>{counts.active}</b>
          <span>active</span>
        </div>
        <div className="chip">
          <span className="dot" style={{ background: "var(--idle)" }} />
          <b>{counts.idle}</b>
          <span>idle</span>
        </div>
        <div className="chip">
          <span className="dot" style={{ background: "var(--frozen)", boxShadow: "0 0 8px rgba(127,205,234,.7)" }} />
          <b>{counts.snapshot}</b>
          <span>frozen</span>
        </div>
        {reclaimed && (
          <div className="chip reclaim">
            <span className="ic">
              <PodIcon size={16} />
            </span>
            <div>
              <b style={{ fontSize: 13, color: "var(--frozen)" }}>{reclaimed}</b>
              <div className="reclaim-lab">reclaimed from frozen</div>
            </div>
          </div>
        )}
      </div>

      {error && <div className="error">Failed to load sessions: {error}</div>}
      {!error && sessions === null && <div className="empty">Loading…</div>}
      {!error && sessions?.length === 0 && (
        <div className="empty">No sessions yet. Create one to get started.</div>
      )}

      <div className="grid">
        {sessions?.map((s) => (
          <SessionCard key={s.id} s={s} now={now} />
        ))}
      </div>
    </div>
  );
}
