// mockup: docs/mockups/restore.html
import { useEffect, useState } from "react";
import { Link, useNavigate, useParams } from "react-router-dom";
import { api } from "../api/client";
import type { Session } from "../api/types";
import { DeleteSessionDialog } from "../app/DeleteSessionDialog";
import { liveSessionPath } from "../app/sessionRoutes";
import { isAgentWorkload, isGatedWorkload } from "../app/workloadKind";
import { useToast } from "../app/Toast";

const AGENT_RESTORE_COPY =
  "A fresh agent pod will restore the conversation history, working directory, " +
  "and accumulated output. No CRIU checkpoint is used.";
const SHELL_RESTORE_COPY =
  "The checkpoint will thaw into a fresh pod and resume the shell exactly where it froze.";
const GATED_RESTORE_COPY =
  "A fresh workload pod and a fresh helper pod will restore the conversation " +
  "history, working directory, shared volume, and accumulated output. The " +
  "approval gate is re-armed; nothing that was pending survives the freeze.";

// 복원 메커니즘의 타입별 분기는 AC-B2 · AC-D4 · AC-E5 · AC-F4/F5 가 정본이다.
export function Restore() {
  const { id = "" } = useParams();
  const [sess, setSess] = useState<Session | null>(null);
  const [error, setError] = useState<string | null>(null);
  const [busy, setBusy] = useState(false);
  const [deleteOpen, setDeleteOpen] = useState(false);
  const navigate = useNavigate();
  const { toast } = useToast();

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

  function handleDeleted(deleted: Session) {
    toast('Session "' + deleted.name + '" deleted');
    navigate("/", { replace: true });
  }

  if (error) return <div className="pad error">Failed to load session: {error}</div>;
  if (!sess) return <div className="pad empty">Loading…</div>;

  const isAgent = isAgentWorkload(sess.workloadType);
  const restoreCopy = isGatedWorkload(sess.workloadType)
    ? GATED_RESTORE_COPY
    : isAgent
      ? AGENT_RESTORE_COPY
      : SHELL_RESTORE_COPY;
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
          {restoreCopy}
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
            type="button"
            className="btn btn-primary"
            data-testid="restore-submit"
            onClick={resume}
            disabled={busy}
          >
            {resumeLabel}
          </button>
          <button
            type="button"
            className="btn btn-danger"
            data-testid="restore-delete-session"
            onClick={() => setDeleteOpen(true)}
            disabled={busy}
          >
            Delete session
          </button>
        </div>
      </div>

      {deleteOpen ? (
        <DeleteSessionDialog
          session={sess}
          onCancel={() => setDeleteOpen(false)}
          onDeleted={handleDeleted}
        />
      ) : null}
    </div>
  );
}
