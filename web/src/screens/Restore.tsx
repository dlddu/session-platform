import { useEffect, useState } from "react";
import { Link, useNavigate, useParams } from "react-router-dom";
import { api } from "../api/client";
import type { Session } from "../api/types";

// Restore — the thaw screen for a snapshotted session. "Resume" calls switch,
// which restores the checkpoint into a new pod (AC-B2, AC-C4) and routes into
// the Workspace. [plan step 5]
export function Restore() {
  const { id = "" } = useParams();
  const [sess, setSess] = useState<Session | null>(null);
  const [error, setError] = useState<string | null>(null);
  const [busy, setBusy] = useState(false);
  const navigate = useNavigate();

  useEffect(() => {
    api.getSession(id).then(setSess).catch((e) => setError(String(e)));
  }, [id]);

  async function resume() {
    setBusy(true);
    try {
      await api.switchSession(id);
      navigate(`/session/${id}`);
    } catch (e) {
      setError(String(e));
      setBusy(false);
    }
  }

  if (error) return <div className="pad error">Failed to load session: {error}</div>;
  if (!sess) return <div className="pad empty">Loading…</div>;

  return (
    <div className="pad">
      <div className="crumbs">
        <Link to="/" className="back">
          ← Sessions
        </Link>
        <span>/</span>
        <span>{sess.name}</span>
      </div>

      <div className="card" data-state="snapshot" style={{ maxWidth: 520, cursor: "default" }}>
        <div className="card-head">
          <div>
            <div className="c-name">{sess.name}</div>
            <div className="c-id">session/{sess.id} · frozen</div>
          </div>
          <span className="badge snapshot">
            <span className="led" />
            snapshot
          </span>
        </div>
        <div style={{ fontFamily: "var(--mono)", color: "var(--frozen)", fontSize: 13 }}>
          {sess.checkpoint ? (
            <>
              checkpoint {sess.checkpoint.ref} · {sess.checkpoint.sizeBytes} bytes
              {sess.checkpoint.reclaimed ? ` · reclaimed ${sess.checkpoint.reclaimed}` : ""}
            </>
          ) : (
            "checkpoint metadata unavailable"
          )}
        </div>
        <div className="modal-actions" style={{ marginTop: 20 }}>
          <button className="btn btn-primary" onClick={resume} disabled={busy}>
            {busy ? "Thawing…" : "Thaw & resume"}
          </button>
        </div>
      </div>
    </div>
  );
}
