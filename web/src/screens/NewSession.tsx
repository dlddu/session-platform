import { useState } from "react";
import { useNavigate } from "react-router-dom";
import { api } from "../api/client";

// NewSession — modal over the Sessions console. Submits POST /api/v1/sessions
// (which provisions a dedicated pod, AC-A1/A2) then routes to the new session's
// Workspace. [plan step 5]
export function NewSession() {
  const [name, setName] = useState("");
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState<string | null>(null);
  const navigate = useNavigate();

  const close = () => navigate("/");

  async function submit(e: React.FormEvent) {
    e.preventDefault();
    if (!name.trim()) return;
    setBusy(true);
    setError(null);
    try {
      const sess = await api.createSession({ name: name.trim() });
      navigate(`/session/${sess.id}`);
    } catch (err) {
      setError(String(err));
      setBusy(false);
    }
  }

  return (
    <div className="scrim" onClick={close}>
      <form className="modal" onClick={(e) => e.stopPropagation()} onSubmit={submit}>
        <h3>New session</h3>
        <p className="desc">
          A dedicated data plane pod is provisioned for this session.
        </p>
        <label className="field">
          <span>Name</span>
          <input
            autoFocus
            data-testid="new-session-name"
            value={name}
            onChange={(e) => setName(e.target.value)}
            placeholder="api-gateway-dev"
          />
        </label>
        {error && <div className="error" style={{ padding: "0 0 12px" }}>{error}</div>}
        <div className="modal-actions">
          <button type="button" className="btn btn-ghost" onClick={close} disabled={busy}>
            Cancel
          </button>
          <button
            type="submit"
            data-testid="new-session-submit"
            className="btn btn-primary"
            disabled={busy || !name.trim()}
          >
            {busy ? "Provisioning…" : "Create"}
          </button>
        </div>
      </form>
    </div>
  );
}
