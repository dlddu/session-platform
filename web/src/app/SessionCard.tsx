import { useEffect, useRef, useState } from "react";
import { useNavigate } from "react-router-dom";
import type { Session } from "../api/types";
import { StateBadge } from "./StateBadge";
import { ClockIcon, CrystalIcon, PodIcon } from "./icons";
import { sessionEntryPath } from "./sessionRoutes";

// MaxIdle mirrors control-plane/internal/session.MaxIdle (60m). A session
// freezes once it has been idle this long; the idle card counts down to it.
const MAX_IDLE_MS = 60 * 60 * 1000;

function fmtSec(total: number): string {
  const m = Math.floor(total / 60);
  const s = total % 60;
  return `${m}:${String(s).padStart(2, "0")}`;
}

// Human-readable checkpoint size from a raw byte count.
function fmtBytes(n: number): string {
  const MB = 1024 * 1024;
  const GB = 1024 * MB;
  if (n >= GB) return `${(n / GB).toFixed(1)} GB`;
  return `${Math.max(1, Math.round(n / MB))} MB`;
}

// Relative "frozen Xm ago" label from an ISO timestamp.
function fmtAgo(iso: string, now: number): string {
  const diffMin = Math.max(0, Math.floor((now - Date.parse(iso)) / 60000));
  if (diffMin < 1) return "just now";
  if (diffMin < 60) return `${diffMin}m ago`;
  const h = Math.floor(diffMin / 60);
  if (h < 24) return `${h}h ago`;
  return `${Math.floor(h / 24)}d ago`;
}

// active vitals — static placeholders. Real CPU/Mem metrics are out of scope
// (the control plane does not emit them yet); this keeps layout parity so the
// values can be dropped in later without touching structure.
function ActiveBody() {
  return (
    <div className="vit" aria-hidden="true">
      <div className="m">
        <div className="lab">CPU</div>
        <div className="val val-empty">—</div>
        <div className="bar">
          <i className="active" style={{ width: 0 }} />
        </div>
      </div>
      <div className="m">
        <div className="lab">Mem</div>
        <div className="val val-empty">—</div>
        <div className="bar">
          <i className="active" style={{ width: 0 }} />
        </div>
      </div>
    </div>
  );
}

// idle freeze countdown — derived purely from lastAccess + MaxIdle, ticked by
// the parent's `now` clock (no refetch).
function IdleBody({ s, now }: { s: Session; now: number }) {
  const idleMs = Math.max(0, now - Date.parse(s.lastAccess));
  const freezeSec = Math.max(0, Math.floor((MAX_IDLE_MS - idleMs) / 1000));
  const idleMin = Math.floor(idleMs / 60000);
  const pct = Math.min(100, Math.max(0, (1 - freezeSec / 3600) * 100));
  return (
    <div className="freeze">
      <div className="ring" style={{ ["--p" as string]: pct }}>
        <b>{Math.round(freezeSec / 60)}m</b>
      </div>
      <div className="txt">
        <div className="t">freezes in {fmtSec(freezeSec)}</div>
        <div className="d">idle {idleMin}m · holding pod</div>
      </div>
    </div>
  );
}

function SnapshotBody({ s, now }: { s: Session; now: number }) {
  const cp = s.checkpoint;
  return (
    <>
      <div className="snap-body">
        <div className="crystal">
          <CrystalIcon size={44} />
        </div>
        <div className="snap-meta">
          <div className="row">
            <span className="k">
              {s.workloadType === "claude-code" ? "archive" : "checkpoint"}
            </span>
            <span className="v">{cp ? fmtBytes(cp.sizeBytes) : "—"}</span>
          </div>
          <div className="row">
            <span className="k">frozen</span>
            <span className="v">{cp ? fmtAgo(cp.createdAt, now) : "—"}</span>
          </div>
          <div className="row">
            <span className="k">reclaimed</span>
            <span className="v">{cp?.reclaimed ?? "—"}</span>
          </div>
        </div>
      </div>
      <div className="snap-hint">
        <ClockIcon size={13} />{" "}
        {s.workloadType === "claude-code"
          ? "open to restore the archive and continue the conversation"
          : "open to thaw and resume exactly where it froze"}
      </div>
    </>
  );
}

// SessionCard — one card in the console grid. Routes to Workspace (live) or
// Restore (snapshot). Preserves the session-card testid / data-* attributes the
// e2e specs key off of.
export function SessionCard({
  s,
  now,
  onRequestDelete,
}: {
  s: Session;
  now: number;
  onRequestDelete: (session: Session) => void;
}) {
  const navigate = useNavigate();
  const [menuOpen, setMenuOpen] = useState(false);
  const menuRef = useRef<HTMLDivElement>(null);
  const triggerRef = useRef<HTMLButtonElement>(null);
  const deleteRef = useRef<HTMLButtonElement>(null);

  useEffect(() => {
    if (!menuOpen) return;

    deleteRef.current?.focus();

    function closeOnPointerDown(event: PointerEvent) {
      if (!menuRef.current?.contains(event.target as Node)) {
        setMenuOpen(false);
      }
    }

    function closeOnEscape(event: KeyboardEvent) {
      if (event.key === "Escape") {
        setMenuOpen(false);
        triggerRef.current?.focus();
      }
    }

    document.addEventListener("pointerdown", closeOnPointerDown);
    document.addEventListener("keydown", closeOnEscape);
    return () => {
      document.removeEventListener("pointerdown", closeOnPointerDown);
      document.removeEventListener("keydown", closeOnEscape);
    };
  }, [menuOpen]);

  return (
    <article
      className="card"
      data-testid="session-card"
      data-session-id={s.id}
      data-state={s.state}
      data-workload-type={s.workloadType}
    >
      <button
        type="button"
        className="card-open"
        onClick={() => navigate(sessionEntryPath(s))}
        aria-label={s.name + ", " + s.workloadType + ", " + s.state}
      >
        {s.state === "snapshot" ? (
          <>
            <div className="frost" />
            <div className="shimmer" />
          </>
        ) : null}
        <div className="card-head">
          <div>
            <div className="c-name">{s.name}</div>
            <div className="c-id">session/{s.id}</div>
          </div>
          <StateBadge state={s.state} />
        </div>

        {s.state === "active" ? <ActiveBody /> : null}
        {s.state === "idle" ? <IdleBody s={s} now={now} /> : null}
        {s.state === "snapshot" ? <SnapshotBody s={s} now={now} /> : null}

        <div className="card-foot">
          {s.state === "snapshot" || !s.pod ? (
            <span className="pod" style={{ color: "#5a7686" }}>
              <PodIcon size={12} /> pod reclaimed
            </span>
          ) : (
            <span className="pod">
              <PodIcon size={12} /> pod/{s.pod}
            </span>
          )}
          <span
            className={
              "workload-tag" +
              (s.workloadType === "claude-code" ? " workload-tag-agent" : "")
            }
            title={"workloadType=" + s.workloadType}
          >
            {s.workloadType === "claude-code" ? "◇ claude-code" : "$ shell"}
          </span>
        </div>
      </button>

      <div
        className="card-menu"
        ref={menuRef}
        onBlur={(event) => {
          if (!event.currentTarget.contains(event.relatedTarget)) {
            setMenuOpen(false);
          }
        }}
      >
        <button
          type="button"
          className="card-menu-trigger"
          aria-label={"Actions for " + s.name}
          aria-haspopup="menu"
          aria-expanded={menuOpen}
          onClick={() => setMenuOpen((open) => !open)}
          ref={triggerRef}
          data-testid="session-actions"
        >
          <svg
            width="18"
            height="18"
            viewBox="0 0 24 24"
            fill="currentColor"
            aria-hidden="true"
          >
            <circle cx="5" cy="12" r="1.7" />
            <circle cx="12" cy="12" r="1.7" />
            <circle cx="19" cy="12" r="1.7" />
          </svg>
        </button>
        {menuOpen ? (
          <div
            className="card-menu-popover"
            role="menu"
            onKeyDown={(event) => {
              if (
                event.key === "ArrowDown" ||
                event.key === "ArrowUp" ||
                event.key === "Home" ||
                event.key === "End"
              ) {
                event.preventDefault();
                deleteRef.current?.focus();
              }
            }}
          >
            <button
              type="button"
              role="menuitem"
              className="card-delete-action"
              onClick={() => {
                triggerRef.current?.focus();
                setMenuOpen(false);
                onRequestDelete(s);
              }}
              ref={deleteRef}
              data-testid="session-delete"
            >
              <svg
                width="15"
                height="15"
                viewBox="0 0 24 24"
                fill="none"
                stroke="currentColor"
                strokeWidth="2"
                strokeLinecap="round"
                strokeLinejoin="round"
                aria-hidden="true"
              >
                <path d="M3 6h18M8 6V4h8v2M6 6l1 15h10l1-15" />
              </svg>
              Delete
            </button>
          </div>
        ) : null}
      </div>
    </article>
  );
}
