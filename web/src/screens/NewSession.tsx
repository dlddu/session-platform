// mockup: docs/mockups/new-session.html
import { useEffect, useRef, useState } from "react";
import { useNavigate } from "react-router-dom";
import { api } from "../api/client";
import type { Session, WorkloadType } from "../api/types";
import { liveSessionPath } from "../app/sessionRoutes";
import { isAgentWorkload, isGatedWorkload } from "../app/workloadKind";

// 세 단계는 **시각적 어포던스일 뿐이다** — 생성 호출은 백엔드에서 원자적이라
// (AC-A1/A2) 이 단계들에 대응하는 서버 상태가 없다.

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
  const [workloadType, setWorkloadType] = useState<WorkloadType>("shell");
  const [model, setModel] = useState("");
  const [submittedModel, setSubmittedModel] = useState("");
  const [defaultModel, setDefaultModel] = useState("platform-default");
  const [configuredModels, setConfiguredModels] = useState<string[]>([]);
  const [phase, setPhase] = useState<"input" | "provisioning">("input");
  const [error, setError] = useState<string | null>(null);
  // 완료 표시된 단계 수(0..3). 이 인덱스의 단계가 「진행 중」이다.
  const [done, setDone] = useState(0);
  const [pod, setPod] = useState<string | null>(null);
  const [session, setSession] = useState<Session | null>(null);
  // Mirror of `session` readable inside the timer closure so the staging tick
  // can short-circuit to done the instant the (atomic) response lands.
  const sessionRef = useRef<Session | null>(null);
  const navigate = useNavigate();
  const reduce = prefersReducedMotion();

  useEffect(() => {
    let active = true;

    void api
      .getConfig()
      .then((config) => {
        if (!active) return;
        const { defaultModel: configuredDefaultModel, models } =
          config.claudeCode;
        const selectableModels = models.filter(
          (configuredModel) => configuredModel !== configuredDefaultModel,
        );
        setDefaultModel(configuredDefaultModel);
        setConfiguredModels(models);
        if (models.length > 0) {
          setModel((current) =>
            current === "" || selectableModels.includes(current) ? current : "",
          );
        }
      })
      .catch(() => {
        // Older/unavailable control planes retain the free-text model input.
        if (active) {
          setDefaultModel("platform-default");
          setConfiguredModels([]);
        }
      });

    return () => {
      active = false;
    };
  }, []);

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
    const trimmedModel = model.trim();
    setSubmittedModel(
      trimmedModel ||
        (defaultModel === "platform-default"
          ? "platform default"
          : defaultModel),
    );
    api
      .createSession({
        name: trimmed,
        workloadType,
        ...(isAgentWorkload(workloadType) && trimmedModel
          ? { model: trimmedModel }
          : {}),
      })
      .then((sess) => {
        sessionRef.current = sess;
        setSession(sess);
      })
      .catch((err) => {
        setError(String(err));
        setPhase("input");
      });
  }

  // reduced motion 이거나 응답이 이미 도착했으면 곧장 완료로 건너뛴다.
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

  useEffect(() => {
    if (!session) return;
    setPod(session.pod ?? "pod scheduled");
    setDone(3);
  }, [session]);

  // 완료 콜아웃이 눈에 보일 만큼만 지연시킨 뒤 워크스페이스로 넘긴다.
  useEffect(() => {
    if (!session || done < 3) return;
    const t = setTimeout(
      () => navigate(liveSessionPath(session)),
      reduce ? 0 : SETTLE_MS,
    );
    return () => clearTimeout(t);
  }, [session, done, reduce, navigate]);

  const concreteDefaultModel =
    defaultModel === "platform-default" ? null : defaultModel;
  const defaultModelLabel = concreteDefaultModel
    ? `${concreteDefaultModel} (platform default)`
    : "Platform default";
  const selectableModels = configuredModels.filter(
    (configuredModel) => configuredModel !== concreteDefaultModel,
  );

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
          Pick a working environment. Either way, this session gets one fully
          isolated, dedicated pod.
        </p>

        {phase === "input" ? (
          <form onSubmit={submit}>
            <label className="field">
              <span>Session name</span>
              <input
                autoFocus
                data-testid="new-session-name"
                value={name}
                onChange={(e) => setName(e.target.value)}
                placeholder="api-gateway-dev"
              />
            </label>
            <fieldset className="workload-field">
              <legend>Workload type</legend>
              <div
                className="workload-types"
                role="radiogroup"
                aria-label="Workload type"
              >
                <button
                  type="button"
                  role="radio"
                  aria-checked={workloadType === "shell"}
                  className={"workload-option" + (workloadType === "shell" ? " selected" : "")}
                  data-testid="new-session-workload-shell"
                  onClick={() => setWorkloadType("shell")}
                >
                  <span className="workload-default">default</span>
                  <span className="workload-icon" aria-hidden="true">$</span>
                  <strong>shell</strong>
                  <small>Interactive bash. You type the commands.</small>
                </button>
                <button
                  type="button"
                  role="radio"
                  aria-checked={workloadType === "claude-code"}
                  className={
                    "workload-option" +
                    (workloadType === "claude-code" ? " selected" : "")
                  }
                  data-testid="new-session-workload-claude-code"
                  onClick={() => setWorkloadType("claude-code")}
                >
                  <span className="workload-icon agent" aria-hidden="true">◇</span>
                  <strong>claude-code</strong>
                  <small>Agent CLI. You send prompts, it does the work.</small>
                </button>
                <button
                  type="button"
                  role="radio"
                  aria-checked={workloadType === "approval-gated"}
                  className={
                    "workload-option wide" +
                    (workloadType === "approval-gated" ? " selected" : "")
                  }
                  data-testid="new-session-workload-approval-gated"
                  onClick={() => setWorkloadType("approval-gated")}
                >
                  <span className="workload-icon gated" aria-hidden="true">⏸</span>
                  <strong>approval-gated</strong>
                  <small>
                    Same agent, but it cannot reach the outside on its own —
                    every outbound call waits for your approval.
                  </small>
                </button>
              </div>
            </fieldset>
            {isAgentWorkload(workloadType) ? (
              <label className="field model-field">
                <span>Model</span>
                {configuredModels.length > 0 ? (
                  <select
                    data-testid="new-session-model"
                    value={model}
                    onChange={(e) => setModel(e.target.value)}
                    aria-describedby="new-session-model-hint"
                  >
                    <option value="">{defaultModelLabel}</option>
                    {selectableModels.map((configuredModel) => (
                      <option key={configuredModel} value={configuredModel}>
                        {configuredModel}
                      </option>
                    ))}
                  </select>
                ) : (
                  <input
                    data-testid="new-session-model"
                    value={model}
                    onChange={(e) => setModel(e.target.value)}
                    placeholder={defaultModelLabel}
                    autoComplete="off"
                    spellCheck={false}
                    aria-describedby="new-session-model-hint"
                  />
                )}
                <small className="field-hint" id="new-session-model-hint">
                  {configuredModels.length > 0
                    ? concreteDefaultModel
                      ? `Choose ${concreteDefaultModel} (platform default) to use the platform default.`
                      : "Choose Platform default to use the platform default."
                    : concreteDefaultModel
                      ? `Leave blank to use ${concreteDefaultModel} (platform default).`
                      : "Leave blank to use the platform default."}
                </small>
              </label>
            ) : null}
            <div className="immutable-note">
              <span aria-hidden="true">▣</span>
              <span>
                {isGatedWorkload(workloadType)
                  ? "Workload type and model choice are fixed for this session. The session also gets a helper pod that holds the approval gate and the only route out."
                  : isAgentWorkload(workloadType)
                    ? "Workload type and model choice are fixed for this session. Platform default resolves to the configured default at container start."
                    : "Workload type is fixed for the lifetime of this session."}
              </span>
            </div>
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
            <div className="provision-workload" data-testid="prov-workload">
              {isAgentWorkload(workloadType)
                ? `workloadType=${workloadType} · model=${submittedModel}`
                : "workloadType=shell"}
            </div>
            <div className="steps" data-testid="prov-steps">
              {STEP_LABELS.map((label, i) => {
                const status = i < done ? "ok" : i === done ? "run" : "";
                const displayLabel =
                  i === 1
                    ? isGatedWorkload(workloadType)
                      ? "Schedule dedicated pod + helper pod (gated agent)"
                      : isAgentWorkload(workloadType)
                        ? "Schedule dedicated pod (agent CLI)"
                        : "Schedule dedicated pod (shell)"
                    : label;
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
                    {displayLabel}
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
