import { useEffect, useRef, useState } from "react";
import { useNavigate } from "react-router-dom";
import { api } from "../api/client";
import type { Session } from "../api/types";

// NewSession — modal over the Sessions console. A two-phase flow: the name
// input, then a staged provisioning view. Submit POSTs /api/v1/sessions (which
// atomically provisions a dedicated pod, AC-A1/A2) then routes to the new
// session's workspace at /session/:id. The three stages are a visual affordance
// only — the create call is atomic on the backend; the stages light up on a
// short timer and snap to fully done the moment the response lands.
// [plan steps 2-6]

const STEP_LABELS = [
  "Register session metadata",
  "Schedule dedicated pod",
  "Open read / write channel",
];

const STEP_GAP_MS = 450;
const SETTLE_MS = 320;

function prefersReducedMotion(): boolean {
  return (
    typeof window !== "undefined" &&
    typeof window.matchMedia === "function" &&
    window.matchMedia("(prefers-reduced-motion: reduce)").matches
  );
}

const hexMark = (
  <svg
    width="22"
    height="22"
    viewBox="0 0 24 24"
    fill="none"
    stroke="currentColor"
    strokeWidth="2"
    strokeLinecap="round"
    strokeLinejoin="round"
  >
    <path d="M12 2 21 7v10l-9 5-9-5V7l9-5Z" />
    <circle cx="12" cy="12" r="2" />
  </svg>
);

const checkIcon = (
  <svg
    width="13"
    height="13"
    viewBox="0 0 24 24"
    fill="none"
    stroke="currentColor"
    strokeWidth="3"
    strokeLinecap="round"
    strokeLinejoin="round"
  >
    <path d="M20 6 9 17l-5-5" />
  </svg>
);

const spinIcon = (
  <span className="spin">
    <svg
      width="13"
      height="13"
      viewBox="0 0 24 24"
      fill="none"
      stroke="currentColor"
      strokeWidth="2.4"
      strokeLinecap="round"
    >
      <path d="M12 3a9 9 0 1 0 9 9" />
    </svg>
  </span>
);

const podIcon = (
  <svg
    width="15"
    height="15"
    viewBox="0 0 24 24"
    fill="none"
    stroke="currentColor"
    strokeWidth="2"
    strokeLinecap="round"
    strokeLinejoin="round"
  >
    <path d="M12 2 21 7v10l-9 5-9-5V7l9-5Z" />
  </svg>
);

export function NewSession() {
  const [name, setName] = useState("");
  const [phase, setPhase] = useState<"input" | "provisioning">("input");
  const [error, setError] = useState<string | null>(null);
  // Number of stages marked done (0..3). The stage at this index is the one
  // currently running; later stages stay pending.
  const [done, setDone] = useState(0);
  const [pod, setPod] = useState<string | null>(null);
  const [session, setSession] = useState<Session | null>(null);
  // Mirror of `session` readable inside the timer closure so the staging tick
  // can short-circuit to done the instant the (atomic) response lands.
  const sessionRef = useRef<Session | null>(null);
  const navigate = useNavigate();
  const reduce = prefersReducedMotion();

  const close = () => {
    if (phase === "provisioning") return;
    navigate("/");
  };

  function submit(e: React.FormEvent) {
    e.preventDefault();
    const trimmed = name.trim();
    if (!trimmed) return;
    setError(null);
    setDone(0);
    setPod(null);
    setSession(null);
    sessionRef.current = null;
    setPhase("provisioning");
    api
      .createSession({ name: trimmed })
      .then((sess) => {
        sessionRef.current = sess;
        setSession(sess);
      })
      .catch((err) => {
        setError(String(err));
        setPhase("input");
      });
  }

  // Stage ticker — lights up the stages on a short timer for affordance. Under
  // reduced motion (or once the response lands) it jumps straight to done.
  useEffect(() => {
    if (phase !== "provisioning") return;
    if (reduce) {
      setDone(3);
      return;
    }
    let n = 0;
    let timer: ReturnType<typeof setTimeout>;
    const tick = () => {
      if (sessionRef.current) {
        setDone(3);
        return;
      }
      n += 1;
      setDone(n);
      if (n < 3) timer = setTimeout(tick, STEP_GAP_MS);
    };
    timer = setTimeout(tick, STEP_GAP_MS);
    return () => clearTimeout(timer);
  }, [phase, reduce]);

  // Response landed: fill the pod callout and mark every stage done.
  useEffect(() => {
    if (!session) return;
    setPod(session.pod ?? "pod scheduled");
    setDone(3);
  }, [session]);

  // Every stage done and the session exists: route into its workspace, after a
  // brief settle so the completed callout is perceptible.
  useEffect(() => {
    if (!session || done < 3) return;
    const t = setTimeout(
      () => navigate(`/session/${session.id}`),
      reduce ? 0 : SETTLE_MS,
    );
    return () => clearTimeout(t);
  }, [session, done, reduce, navigate]);

  return (
    <div className="scrim" onClick={close}>
      <div
        className="modal"
        role="dialog"
        aria-modal="true"
        aria-label="Create session"
        onClick={(e) => e.stopPropagation()}
      >
        <div className="m-mark">{hexMark}</div>
        <h3>New session</h3>
        <p className="desc">
          A dedicated pod is provisioned for this session — one session, one pod,
          fully isolated.
        </p>

        {phase === "input" ? (
          <form onSubmit={submit}>
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
            {error && (
              <div className="error" style={{ padding: "0 0 12px" }}>
                {error}
              </div>
            )}
            <div className="modal-actions">
              <button type="button" className="btn btn-ghost" onClick={close}>
                Cancel
              </button>
              <button
                type="submit"
                data-testid="new-session-submit"
                className="btn btn-primary"
                disabled={!name.trim()}
              >
                Create
              </button>
            </div>
          </form>
        ) : (
          <>
            <div className="steps" data-testid="prov-steps">
              {STEP_LABELS.map((label, i) => {
                const status = i < done ? "ok" : i === done ? "run" : "";
                return (
                  <div key={label} className={`step ${status}`.trimEnd()}>
                    <span className="ico">
                      {status === "ok" ? (
                        checkIcon
                      ) : status === "run" ? (
                        spinIcon
                      ) : (
                        <span className="n">{i + 1}</span>
                      )}
                    </span>
                    {label}
                    <span className="mono done-t">
                      {status === "ok" ? "done" : ""}
                    </span>
                  </div>
                );
              })}
            </div>
            <div className={`pod-callout${pod ? " show" : ""}`}>
              {podIcon}
              <span id="prov-pod-name">
                {pod ? `pod/${pod} scheduled` : "pod scheduled"}
              </span>
            </div>
          </>
        )}
      </div>
    </div>
  );
}
