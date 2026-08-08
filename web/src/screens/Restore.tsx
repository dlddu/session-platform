import { useEffect, useState } from "react";
import { Link, useNavigate, useParams } from "react-router-dom";
import { api } from "../api/client";
import type { Session } from "../api/types";
import { liveSessionPath } from "../app/sessionRoutes";

const AGENT_RESTORE_COPY =
  "A fresh agent pod will restore the conversation history, working directory, " +
  "and accumulated output. No CRIU checkpoint is used.";
const SHELL_RESTORE_COPY =
  "The checkpoint will thaw into a fresh pod and resume the shell exactly where it froze.";

// Restore — the resume screen for a snapshotted session. Shell sessions thaw a
// CRIU checkpoint; claude-code sessions restore a filesystem archive. switch
// returns the active session and its immutable workload picks the live route.
export function Restore() {
  const { id = "" } = useParams();
  const [sess, setSess] = useState<Session | null>(null);
  const [error, setError] = useState<string | null>(null);
  const [busy, setBusy] = useState(false);
  const navigate = useNavigate();

  useEffect(() => {
    let cancelled = false;
    setSess(null);
    setError(null);
    api
      .getSession(id)
      .then((session) => {
        if (!cancelled) setSess(session);
      })
      .catch((requestError) => {
        if (!cancelled) setError(String(requestError));
      });
    return () => {
      cancelled = true;
    };
  }, [id]);

  async function resume() {
    setBusy(true);
    try {
      const active = await api.switchSession(id);
      navigate(liveSessionPath(active));
    } catch (e) {
      setError(String(e));
      setBusy(false);
    }
  }

  if (error) return <div className="pad error">Failed to load session: {error}</div>;
  if (!sess) return <div className="pad empty">Loading…</div>;

  const isAgent = sess.workloadType === "claude-code";
  const resumeLabel = busy
    ? isAgent
      ? "Restoring…"
      : "Thawing…"
    : isAgent
      ? "Restore & resume"
      : "Thaw & resume";

  return (
    <div className="pad">
      <div className="crumbs">
        <Link to="/" className="back">
          ← Sessions
        </Link>
        <span>/</span>
        <span>{sess.name}</span>
      </div>

      <div
        className="card restore-card"
        data-state="snapshot"
        data-workload-type={sess.workloadType}
      >
        <div className="card-head">
          <div>
            <div className="c-name">{sess.name}</div>
            <div className="c-id">
              session/{sess.id} · {isAgent ? "archived" : "frozen"}
            </div>
          </div>
          <span className="badge snapshot">
            <span className="led" />
            snapshot
          </span>
        </div>
        <h2 className="restore-title">
          {isAgent ? "Resume from session archive" : "Resume from checkpoint"}
        </h2>
        <p className="restore-copy">
          {isAgent ? AGENT_RESTORE_COPY : SHELL_RESTORE_COPY}
        </p>
        <div className="restore-meta">
          {sess.checkpoint ? (
            <>
              {isAgent ? "archive" : "checkpoint"} {sess.checkpoint.ref} ·{" "}
              {sess.checkpoint.sizeBytes} bytes
              {sess.checkpoint.reclaimed ? ` · reclaimed ${sess.checkpoint.reclaimed}` : ""}
            </>
          ) : (
            "checkpoint metadata unavailable"
          )}
        </div>
        <div className="modal-actions restore-actions">
          <button
            className="btn btn-primary"
            data-testid="restore-submit"
            onClick={resume}
            disabled={busy}
          >
            {resumeLabel}
          </button>
        </div>
      </div>
    </div>
  );
}
