// mockup: docs/mockups/repo-access.html
import { useCallback, useEffect, useRef, useState, type FormEvent } from "react";
import { ApiError, api } from "../api/client";
import type { AccessRequest, AllowlistEntry, AuthUser, RepoAccessState } from "../api/types";
import { REPO_ACCESS_PATH, useRepoAccessBadge } from "../app/AppShell";
import { LockIcon } from "../app/icons";
import { useToast } from "../app/Toast";
import "./RepoAccess.css";

const REPO_RE = /^[A-Za-z0-9_.-]+\/[A-Za-z0-9_.-]+$/;
const DISABLED_MESSAGE = "Repo access is not enabled on this deployment.";
const SIGNIN_UNCONFIGURED_MESSAGE = "Sign-in is not configured on this deployment.";

type Phase =
  | { kind: "loading" }
  | { kind: "signed-out" }
  | { kind: "disabled"; message: string }
  | { kind: "error"; message: string }
  | { kind: "ready"; user: AuthUser; data: RepoAccessState };

function statusOf(e: unknown): number | null {
  return e instanceof ApiError ? e.status : null;
}

function errorText(e: unknown): string {
  if (e instanceof ApiError) return e.detail;
  if (e instanceof Error) return e.message;
  return String(e);
}

function shortId(id: string): string {
  return id.slice(0, 8);
}

function fmtAgo(iso: string, now: number): string {
  const t = Date.parse(iso);
  if (Number.isNaN(t)) return "";
  const min = Math.max(0, Math.floor((now - t) / 60000));
  if (min < 1) return "just now";
  if (min < 60) return `${min} min ago`;
  const h = Math.floor(min / 60);
  if (h < 24) return `${h} h ago`;
  return `${Math.floor(h / 24)} d ago`;
}

function fmtDate(iso: string): string {
  const d = new Date(iso);
  if (Number.isNaN(d.getTime())) return iso;
  const pad = (n: number) => String(n).padStart(2, "0");
  return `${d.getFullYear()}-${pad(d.getMonth() + 1)}-${pad(d.getDate())}`;
}

function RepoName({ repo }: { repo: string }) {
  const slash = repo.indexOf("/");
  if (slash < 0) return <>{repo}</>;
  return (
    <>
      <span className="owner">{repo.slice(0, slash + 1)}</span>
      {repo.slice(slash + 1)}
    </>
  );
}

function RequestCard({
  r,
  now,
  disabled,
  onDecide,
}: {
  r: AccessRequest;
  now: number;
  disabled: boolean;
  onDecide: (r: AccessRequest, kind: "approve" | "reject") => void;
}) {
  return (
    <article className={"req" + (r.alreadyAllowed ? " already" : "")} data-testid="access-request">
      <div>
        <div className="req-repo">
          <RepoName repo={r.repo} />
        </div>
        <div className="req-meta">
          <span>
            from <b>{r.sessionName || shortId(r.sessionId)}</b>
            {r.sessionName ? (
              <>
                {" "}
                <span className="mono">{shortId(r.sessionId)}</span>
              </>
            ) : null}
          </span>
          {r.workloadType ? <span className="tag">{r.workloadType}</span> : null}
          <span title={r.requestedAt}>
            requested <b>{fmtAgo(r.requestedAt, now)}</b>
          </span>
        </div>
        <div className="req-reason">{r.reason}</div>
      </div>
      <div className="req-actions">
        <div className="row">
          <button
            className="btn btn-ghost btn-sm"
            disabled={disabled}
            onClick={() => onDecide(r, "reject")}
          >
            Reject
          </button>
          <button
            className="btn btn-primary btn-sm"
            disabled={disabled}
            onClick={() => onDecide(r, "approve")}
          >
            Allow
          </button>
        </div>
        <span className="note">
          {r.alreadyAllowed ? "Already allowed" : "Adds this one repository, read-only"}
        </span>
      </div>
    </article>
  );
}

function AllowedRow({
  e,
  disabled,
  onRemove,
}: {
  e: AllowlistEntry;
  disabled: boolean;
  onRemove: (repo: string) => void;
}) {
  return (
    <div className="t-row" data-testid="allowlist-entry">
      <span className="t-repo">{e.repo}</span>
      <span className="t-src">
        {e.addedBy} ·{" "}
        {e.source === "request" ? (
          e.requestSessionId ? (
            <>
              request from <span className="mono">{shortId(e.requestSessionId)}</span>
            </>
          ) : (
            <span className="mono">request</span>
          )
        ) : (
          <span className="mono">manual</span>
        )}
      </span>
      <span className="t-when">{fmtDate(e.addedAt)}</span>
      <span className="t-act">
        <button className="btn btn-danger btn-sm" disabled={disabled} onClick={() => onRemove(e.repo)}>
          Remove
        </button>
      </span>
    </div>
  );
}

function Header({ user, busy, onSignOut }: { user?: AuthUser; busy?: boolean; onSignOut?: () => void }) {
  return (
    <div className="h-top">
      <div>
        <div className="eyebrow">Control plane · Repo access</div>
        <h1>Repo access</h1>
        <div className="sub">
          Private repositories the session MCP may clone. Each entry opens exactly one repository,
          read-only. Every clone still needs its own approval.
        </div>
      </div>
      {user ? (
        <div className="who">
          Signed in as <b>{user.user}</b>
          <button className="btn btn-ghost btn-sm" disabled={busy} onClick={onSignOut}>
            Sign out
          </button>
        </div>
      ) : null}
    </div>
  );
}

export function RepoAccess() {
  const [phase, setPhase] = useState<Phase>({ kind: "loading" });
  const [busy, setBusy] = useState<string | null>(null);
  const [draft, setDraft] = useState("");
  const setBadge = useRepoAccessBadge();
  const { toast } = useToast();
  const alive = useRef(true);

  const show = useCallback(
    (next: Phase) => {
      if (!alive.current) return;
      setPhase(next);
      setBadge(next.kind === "ready" ? next.data.requests.length : null);
    },
    [setBadge],
  );

  const load = useCallback(async () => {
    const [me, access] = await Promise.allSettled([api.getMe(), api.getRepoAccess()]);
    if (access.status === "rejected" && statusOf(access.reason) === 503) {
      show({ kind: "disabled", message: DISABLED_MESSAGE });
    } else if (me.status === "rejected" && statusOf(me.reason) === 503) {
      show({ kind: "disabled", message: SIGNIN_UNCONFIGURED_MESSAGE });
    } else if (
      (me.status === "rejected" && statusOf(me.reason) === 401) ||
      (access.status === "rejected" && statusOf(access.reason) === 401)
    ) {
      show({ kind: "signed-out" });
    } else if (me.status === "rejected") {
      show({ kind: "error", message: errorText(me.reason) });
    } else if (access.status === "rejected") {
      show({ kind: "error", message: errorText(access.reason) });
    } else {
      show({ kind: "ready", user: me.value, data: access.value });
    }
  }, [show]);

  useEffect(() => {
    alive.current = true;
    load();
    return () => {
      alive.current = false;
    };
  }, [load]);

  const reloadAccess = useCallback(
    async (user: AuthUser) => {
      try {
        show({ kind: "ready", user, data: await api.getRepoAccess() });
      } catch (e) {
        const status = statusOf(e);
        if (status === 401) show({ kind: "signed-out" });
        else if (status === 503) show({ kind: "disabled", message: DISABLED_MESSAGE });
        else toast(errorText(e), "warm");
      }
    },
    [show, toast],
  );

  const run = useCallback(
    async (key: string, user: AuthUser, action: () => Promise<unknown>, okMsg: string) => {
      setBusy(key);
      try {
        await action();
        toast(okMsg);
        await reloadAccess(user);
        return true;
      } catch (e) {
        toast(errorText(e), "warm");
        const status = statusOf(e);
        if (status === 401) show({ kind: "signed-out" });
        else if (status === 404) await reloadAccess(user);
        return false;
      } finally {
        if (alive.current) setBusy(null);
      }
    },
    [reloadAccess, show, toast],
  );

  function signIn() {
    window.location.assign(api.loginUrl(REPO_ACCESS_PATH));
  }

  if (phase.kind === "loading") {
    return (
      <div className="pad access">
        <div className="empty">Loading…</div>
      </div>
    );
  }

  if (phase.kind === "signed-out") {
    return (
      <div className="pad access">
        <section className="signin" aria-label="Sign in" data-testid="repo-access-signin">
          <div className="lock">
            <LockIcon size={24} />
          </div>
          <h2>Sign in to manage repo access</h2>
          <p>
            Allowing a repository lets sessions clone it with a token minted for that repository
            alone. Only signed-in users can review requests or change the list.
          </p>
          <button className="btn btn-primary" onClick={signIn}>
            Sign in with Authelia
          </button>
          <div className="fine">Sessions and their workspaces stay available without signing in.</div>
        </section>
      </div>
    );
  }

  if (phase.kind === "disabled" || phase.kind === "error") {
    return (
      <div className="pad access">
        <Header />
        {phase.kind === "disabled" ? (
          <div className="empty" data-testid="repo-access-disabled">
            {phase.message}
          </div>
        ) : (
          <div className="error">
            Failed to load repo access: {phase.message}
            <div>
              <button className="btn btn-ghost btn-sm" onClick={() => load()}>
                Retry
              </button>
            </div>
          </div>
        )}
      </div>
    );
  }

  const { user, data } = phase;
  const now = Date.now();
  const inFlight = busy !== null;

  function decide(r: AccessRequest, kind: "approve" | "reject") {
    if (kind === "approve") {
      run("req:" + r.id, user, () => api.approveRequest(r.id), `${r.repo} allowed (read-only)`);
    } else {
      run("req:" + r.id, user, () => api.rejectRequest(r.id), `Request for ${r.repo} rejected`);
    }
  }

  function remove(repo: string) {
    run("rm:" + repo, user, () => api.removeAllowlist(repo), `${repo} removed — next clone will be refused`);
  }

  async function add(ev: FormEvent) {
    ev.preventDefault();
    const repo = draft.trim();
    if (!REPO_RE.test(repo)) {
      toast("Use owner/repo", "warm");
      return;
    }
    if (await run("add", user, () => api.addAllowlist(repo), `${repo} allowed (read-only)`)) {
      if (alive.current) setDraft("");
    }
  }

  async function signOut() {
    setBusy("logout");
    try {
      await api.logout();
      show({ kind: "signed-out" });
    } catch (e) {
      toast(errorText(e), "warm");
    } finally {
      if (alive.current) setBusy(null);
    }
  }

  return (
    <div className="pad access">
      <Header user={user} busy={inFlight} onSignOut={signOut} />

      <div className="summary">
        <div className="chip">
          <span className="dot pending" />
          <b data-testid="repo-access-pending">{data.requests.length}</b>
          <span>Pending</span>
        </div>
        <div className="chip">
          <span className="dot allowed" />
          <b data-testid="repo-access-allowed">{data.allowlist.length}</b>
          <span>Allowed</span>
        </div>
        <div className="chip scope">owner/repo · contents: read · token minted per clone</div>
      </div>

      <section className="block" aria-label="Pending requests">
        <div className="sec-head">
          <h2>Pending requests</h2>
          <span className="hint">Sessions asked to clone these. Nothing opens until you allow it.</span>
        </div>
        {data.requests.length ? (
          <div className="req-list">
            {data.requests.map((r) => (
              <RequestCard key={r.id} r={r} now={now} disabled={inFlight} onDecide={decide} />
            ))}
          </div>
        ) : (
          <div className="empty">No pending requests.</div>
        )}
      </section>

      <section className="block" aria-label="Allowed repositories">
        <div className="sec-head">
          <h2>Allowed repositories</h2>
          <span className="hint">Removing an entry takes effect on the next clone.</span>
        </div>
        <div className="table">
          <div className="t-row t-head">
            <span>Repository</span>
            <span>Added by</span>
            <span>Added</span>
            <span />
          </div>
          {data.allowlist.map((e) => (
            <AllowedRow key={e.repo} e={e} disabled={inFlight} onRemove={remove} />
          ))}
          <form className="add" onSubmit={add}>
            <label className="add-field">
              <span className="pre">github.com/</span>
              <input
                value={draft}
                onChange={(ev) => setDraft(ev.target.value)}
                placeholder="owner/repo"
                autoComplete="off"
                spellCheck={false}
                aria-label="Repository (owner/repo)"
                data-testid="allowlist-add-input"
              />
            </label>
            <span className="scope-note">read-only · one repository</span>
            <button className="btn btn-ghost btn-sm" type="submit" disabled={inFlight}>
              Add
            </button>
          </form>
        </div>
      </section>
    </div>
  );
}
