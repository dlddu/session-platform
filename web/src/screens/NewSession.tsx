// mockup: docs/mockups/new-session.html
import { useEffect, useRef, useState } from "react";
import { useNavigate } from "react-router-dom";
import { api } from "../api/client";
import type {
  ModelCatalog,
  PlatformConfig,
  Session,
  TenantConfig,
  WorkloadType,
} from "../api/types";
import { LockIcon } from "../app/icons";
import { liveSessionPath } from "../app/sessionRoutes";
import {
  hasModelChoice,
  isAgentWorkload,
  isGatedWorkload,
} from "../app/workloadKind";

// 세 단계는 **시각적 어포던스일 뿐이다** — 생성 호출은 백엔드에서 원자적이라
// (AC-A1/A2) 이 단계들에 대응하는 서버 상태가 없다.

const STEP_LABELS = [
  "Register session metadata",
  "Schedule dedicated pod",
  "Open read / write channel",
];

// Card order follows the control plane's catalog (session.WorkloadTypes).
const WORKLOAD_ORDER: WorkloadType[] = [
  "shell",
  "claude-code",
  "minimax-code",
  "codex",
  "approval-gated",
];

const WORKLOAD_CARDS: Record<
  WorkloadType,
  { icon: string; iconClass?: string; wide?: boolean; description: string }
> = {
  shell: { icon: "$", description: "Interactive bash. You type the commands." },
  "claude-code": {
    icon: "◇",
    iconClass: "agent",
    description: "Agent CLI. You send prompts, it does the work.",
  },
  "minimax-code": {
    icon: "◇",
    iconClass: "agent",
    wide: true,
    description:
      "MiniMax Code CLI on the same prompt loop. Runs the platform's default MiniMax model.",
  },
  codex: {
    icon: "◇",
    iconClass: "agent",
    wide: true,
    description:
      "Codex SDK agent on your ChatGPT plan. Conversation, plugin skills and K3s MCP only.",
  },
  "approval-gated": {
    icon: "⏸",
    iconClass: "gated",
    wide: true,
    description:
      "Same agent, but it cannot reach the outside on its own — every outbound call waits for your approval.",
  },
};

const PLATFORM_DEFAULT_CATALOG: ModelCatalog = {
  defaultModel: "platform-default",
  models: [],
};

// claude-code's catalog is the only one an older control plane reports, and
// approval-gated runs the same agent; any other type falls back to free text.
function catalogFor(
  config: PlatformConfig | null,
  workloadType: WorkloadType,
): ModelCatalog {
  if (!config) return PLATFORM_DEFAULT_CATALOG;
  const entry = config.workloadTypes.find((t) => t.type === workloadType);
  if (entry && entry.defaultModel !== null) {
    return { defaultModel: entry.defaultModel, models: entry.models };
  }
  if (workloadType === "claude-code" || workloadType === "approval-gated") {
    return config.claudeCode;
  }
  return PLATFORM_DEFAULT_CATALOG;
}

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
  const [config, setConfig] = useState<PlatformConfig | null>(null);
  const [tenant, setTenant] = useState("");
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
      .then((loaded) => {
        if (active) setConfig(loaded);
      })
      .catch(() => {
        // Older/unavailable control planes retain the free-text model input.
        if (active) setConfig(null);
      });

    return () => {
      active = false;
    };
  }, []);

  const { defaultModel, models: configuredModels } = catalogFor(
    config,
    workloadType,
  );
  const tenants = config?.tenants ?? [];
  const callerTenant = config?.tenant ?? null;
  const pickedTenant: TenantConfig | null = callerTenant
    ? callerTenant
    : (tenants.find((t) => t.name === tenant) ?? null);
  const offeredTypes = pickedTenant
    ? WORKLOAD_ORDER.filter((t) => pickedTenant.workloadTypes.includes(t))
    : WORKLOAD_ORDER;

  // A pending choice survives a catalog that still lists it; anything else
  // falls back to the platform default.
  useEffect(() => {
    if (configuredModels.length === 0) return;
    setModel((current) =>
      current === "" ||
      (current !== defaultModel && configuredModels.includes(current))
        ? current
        : "",
    );
  }, [configuredModels, defaultModel]);

  useEffect(() => {
    if (offeredTypes.length > 0 && !offeredTypes.includes(workloadType)) {
      setWorkloadType(offeredTypes[0]);
    }
  }, [offeredTypes, workloadType]);

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
    const choosesModel = hasModelChoice(workloadType);
    const trimmedModel = choosesModel ? model.trim() : "";
    setSubmittedModel(
      trimmedModel ||
        (!choosesModel || defaultModel === "platform-default"
          ? "platform default"
          : defaultModel),
    );
    api
      .createSession({
        name: trimmed,
        workloadType,
        ...(choosesModel && trimmedModel ? { model: trimmedModel } : {}),
        ...(!callerTenant && pickedTenant ? { tenant: pickedTenant.name } : {}),
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
          Pick the working environment for this session. Whichever you pick, it
          runs on its own dedicated pod.
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
            {callerTenant ? (
              <div className="tenant-line" data-testid="new-session-tenant-fixed">
                Tenant: <b>{callerTenant.name}</b>
              </div>
            ) : tenants.length > 0 ? (
              <label className="field">
                <span>Tenant</span>
                <select
                  data-testid="new-session-tenant"
                  value={tenant}
                  onChange={(e) => setTenant(e.target.value)}
                  aria-describedby="new-session-tenant-hint"
                >
                  <option value="">No tenant</option>
                  {tenants.map((t) => (
                    <option key={t.name} value={t.name}>
                      {t.name}
                    </option>
                  ))}
                </select>
                <small className="field-hint" id="new-session-tenant-hint">
                  {pickedTenant
                    ? `Only the types ${pickedTenant.name} is granted are shown. The tenant can see and use this session.`
                    : "No tenant keeps the session operator-only."}
                </small>
              </label>
            ) : null}
            <fieldset className="workload-field">
              <legend>Workload type</legend>
              <div
                className="workload-types"
                role="radiogroup"
                aria-label="Workload type"
              >
                {offeredTypes.map((type) => {
                  const card = WORKLOAD_CARDS[type];
                  return (
                    <button
                      key={type}
                      type="button"
                      role="radio"
                      aria-checked={workloadType === type}
                      className={
                        "workload-option" +
                        (card.wide ? " wide" : "") +
                        (workloadType === type ? " selected" : "")
                      }
                      data-testid={`new-session-workload-${type}`}
                      onClick={() => setWorkloadType(type)}
                    >
                      {type === "shell" ? (
                        <span className="workload-default">default</span>
                      ) : null}
                      <span
                        className={
                          "workload-icon" + (card.iconClass ? " " + card.iconClass : "")
                        }
                        aria-hidden="true"
                      >
                        {card.icon}
                      </span>
                      <strong>{type}</strong>
                      <small>{card.description}</small>
                    </button>
                  );
                })}
              </div>
            </fieldset>
            {hasModelChoice(workloadType) ? (
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
              <LockIcon size={14} className="immutable-note-icon" />
              <span>
                {isGatedWorkload(workloadType)
                  ? "Workload type and model choice are fixed for this session. The session also gets a helper pod that holds the approval gate and the only route out."
                  : workloadType === "minimax-code"
                    ? "Workload type is fixed for this session. It runs the platform default MiniMax model, resolved at container start."
                    : workloadType === "codex"
                    ? "Workload type and model choice are fixed for this session. The first prompt signs the session in to ChatGPT, and the login is kept in its archive."
                    : isAgentWorkload(workloadType)
                      ? "Workload type and model choice are fixed for this session. Platform default resolves to the configured default at container start."
                      : (
                        <>
                          Workload type is <b>fixed for the lifetime</b> of the
                          session. To use a different one, create a new
                          session.
                        </>
                      )}
              </span>
            </div>
            <div className="guarantee">
              <b>Same guarantees whichever you pick</b> — dedicated pod
              isolation, idle reclaim, restore-on-access, free switching, and
              consistent state apply to every workload type. Only the freeze
              mechanism, and whether outbound calls need your approval, differ.
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
