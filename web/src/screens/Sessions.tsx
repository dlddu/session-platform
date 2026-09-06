// mockup: docs/mockups/index.html
import { useCallback, useEffect, useState } from "react";
import { Link } from "react-router-dom";
import { api } from "../api/client";
import type { Session } from "../api/types";
import { DeleteSessionDialog } from "../app/DeleteSessionDialog";
import { SessionCard } from "../app/SessionCard";
import { PodIcon } from "../app/icons";
import { useToast } from "../app/Toast";

// checkpoint 의 `reclaimed` 는 "N vCPU · M GB" 자유 문자열이라 파싱해서 합친다.
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

export function Sessions() {
  const [sessions, setSessions] = useState<Session[] | null>(null);
  const [error, setError] = useState<string | null>(null);
  const [now, setNow] = useState(() => Date.now());
  const [sessionToDelete, setSessionToDelete] = useState<Session | null>(null);
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

  // 카운트다운은 이 시계로만 움직인다 — 다시 조회하지 않는다.
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

  const removeDeletedSession = useCallback(
    (deleted: Session) => {
      setSessions((current) =>
        current ? current.filter((session) => session.id !== deleted.id) : current,
      );
      setSessionToDelete(null);
      toast('Session "' + deleted.name + '" deleted');
    },
    [toast],
  );

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
            Every session runs in its own dedicated pod. Idle sessions preserve
            their state and hand their compute back.
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
          <SessionCard
            key={s.id}
            s={s}
            now={now}
            onRequestDelete={setSessionToDelete}
          />
        ))}
      </div>

      {sessionToDelete ? (
        <DeleteSessionDialog
          key={sessionToDelete.id}
          session={sessionToDelete}
          onCancel={() => setSessionToDelete(null)}
          onDeleted={removeDeletedSession}
        />
      ) : null}
    </div>
  );
}
