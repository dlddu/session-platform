import { useCallback, useEffect, useRef, useState } from "react";
import { Link, useParams } from "react-router-dom";
import { api } from "../api/client";
import type { Session } from "../api/types";
import { StateBadge } from "../app/StateBadge";

const READ_DELAYS_MS = [250, 700] as const;

function sleep(ms: number): Promise<void> {
  return new Promise((resolve) => setTimeout(resolve, ms));
}

function appendLine(current: string, line: string): string {
  const separator = current === "" || current.endsWith("\n") ? "" : "\n";
  return current + separator + line + "\n";
}

function formatAgentOutput(payload: string): string {
  const body = payload.replace(/\n+$/, "");
  if (!body) return "";
  return (
    body
      .split("\n")
      .map((line, index) => `${index === 0 ? "‹agent› " : "        "}${line}`)
      .join("\n") + "\n"
  );
}

function displayModel(model?: string): string {
  return !model || model === "platform-default" ? "platform default" : model;
}

// Workspace dispatches its interaction model from the immutable workloadType.
// Shell keeps J5's stdin/stdout loop. Claude Code sends one prompt per write,
// without a synthetic newline, and exposes an explicit output refresh because a
// one-shot agent run may outlive the short bounded reads after submission.
export function Workspace() {
  const { id = "" } = useParams();
  const [sess, setSess] = useState<Session | null>(null);
  const [error, setError] = useState<string | null>(null);
  const [term, setTerm] = useState("");
  const [cmd, setCmd] = useState("");
  const [pendingRuns, setPendingRuns] = useState(0);
  const [reading, setReading] = useState(false);
  const offsetRef = useRef(0);
  const readQueueRef = useRef<Promise<void>>(Promise.resolve());
  const termRef = useRef<HTMLDivElement>(null);
  const activeIdRef = useRef(id);
  activeIdRef.current = id;

  const appendSys = useCallback((line: string) => {
    setTerm((current) => appendLine(current, `◆ ${line}`));
  }, []);

  // Serialize cursor reads so a manual refresh and submission follow-up cannot
  // issue the same offset concurrently and append duplicate output.
  const readDelta = useCallback((): Promise<void> => {
    const request = readQueueRef.current.then(async () => {
      if (activeIdRef.current !== id) return;
      setReading(true);
      try {
        const result = await api.readSession(id, offsetRef.current);
        if (activeIdRef.current !== id) return;
        offsetRef.current = result.nextOffset;
        if (result.payload) {
          setTerm((current) =>
            current +
            (result.session.workloadType === "claude-code"
              ? formatAgentOutput(result.payload)
              : result.payload),
          );
        }
        setSess(result.session);
      } finally {
        if (activeIdRef.current === id) setReading(false);
      }
    });
    readQueueRef.current = request.then(
      () => undefined,
      () => undefined,
    );
    return request;
  }, [id]);

  useEffect(() => {
    let cancelled = false;
    offsetRef.current = 0;
    readQueueRef.current = Promise.resolve();
    setTerm("");
    setCmd("");
    setPendingRuns(0);
    setSess(null);
    setError(null);

    const getRequest = api.getSession(id);
    const readRequest = readDelta();

    void Promise.allSettled([getRequest, readRequest]).then(
      ([getResult, readResult]) => {
        if (cancelled) return;
        if (readResult.status === "rejected") {
          appendSys(`read failed: ${readResult.reason}`);
          if (getResult.status === "fulfilled") {
            setSess(getResult.value);
          } else {
            setError(String(getResult.reason));
          }
        }
      },
    );

    return () => {
      cancelled = true;
    };
  }, [id, readDelta, appendSys]);

  useEffect(() => {
    const element = termRef.current;
    if (element) element.scrollTop = element.scrollHeight;
  }, [term]);

  async function refreshOutput() {
    try {
      await readDelta();
    } catch (readError) {
      appendSys(`read failed: ${readError}`);
    }
  }

  async function run() {
    const command = cmd;
    const isAgent = sess?.workloadType === "claude-code";
    setCmd("");

    if (command.trim() === "") {
      await refreshOutput();
      return;
    }

    if (isAgent) {
      setTerm((current) => appendLine(current, `▸ ${command}`));
      setPendingRuns((count) => count + 1);
    }

    try {
      await api.writeSession(id, isAgent ? command : command + "\n");
      await readDelta();
      for (const delay of READ_DELAYS_MS) {
        await sleep(delay);
        await readDelta();
      }
    } catch (ioError) {
      appendSys(`${isAgent ? "agent" : "shell"} i/o failed: ${ioError}`);
    } finally {
      if (isAgent) {
        setPendingRuns((count) => Math.max(0, count - 1));
      }
    }
  }

  async function doSwitch() {
    try {
      const active = await api.switchSession(id);
      setSess(active);
      appendSys(`switch → ${active.state}`);
    } catch (switchError) {
      appendSys(`switch failed: ${switchError}`);
    }
  }

  if (error) return <div className="pad error">Failed to load session: {error}</div>;
  if (!sess) return <div className="pad empty">Loading…</div>;

  const isAgent = sess.workloadType === "claude-code";
  const frozen = sess.state === "snapshot";
  const modelLabel = displayModel(sess.model?.trim());

  return (
    <div className="pad" data-workload-type={sess.workloadType}>
      <div className="crumbs">
        <Link to="/" className="back">
          ← Sessions
        </Link>
        <span>/</span>
        <span>{sess.name}</span>
      </div>

      <div className="h-top">
        <div>
          <h1>{sess.name}</h1>
          <div className="c-id">
            session/{sess.id} · {sess.pod ? `pod/${sess.pod}` : "pod reclaimed"} ·{" "}
            workloadType={sess.workloadType}
          </div>
        </div>
        <StateBadge state={sess.state} />
      </div>

      <div className="ws-body">
        <div className={"console" + (isAgent ? " agent-console" : "")}>
          <div className="console-bar">
            <span>{isAgent ? "claude-code session" : "session shell"}</span>
            <span data-testid="ws-console-pod">
              {sess.pod ? `pod/${sess.pod}` : "pod reclaimed"}
            </span>
            {isAgent ? (
              <span className="agent-model" data-testid="ws-model">
                model={modelLabel}
              </span>
            ) : null}
            <span className="console-actions">
              {!frozen ? (
                <span className="tag">
                  <span className="led" />
                  {isAgent ? "agent ready" : "shell attached"}
                </span>
              ) : null}
              <button
                type="button"
                className="console-refresh"
                data-testid="ws-refresh-output"
                onClick={() => void refreshOutput()}
                disabled={reading}
              >
                {reading ? "Refreshing…" : "Refresh output"}
              </button>
            </span>
          </div>
          <div
            className="term"
            data-testid="ws-log"
            ref={termRef}
            aria-live="polite"
          >
            {term === "" ? (
              <span className="term-empty">
                {isAgent
                  ? "// ready — send a prompt or refresh output to read agent responses"
                  : "// attached — the shell's output since session start appears here"}
              </span>
            ) : (
              term
            )}
          </div>
          <form
            className="input-row"
            onSubmit={(event) => {
              event.preventDefault();
              void run();
            }}
          >
            <span className={"p" + (isAgent ? " agent-prompt" : "")}>
              {isAgent ? "▸" : "$"}
            </span>
            <input
              data-testid={isAgent ? "ws-prompt" : "ws-cmd"}
              value={cmd}
              onChange={(event) => setCmd(event.target.value)}
              placeholder={
                isAgent
                  ? "send a prompt — runs claude -p once in the session pod"
                  : "run a command — executes in the session shell (bash)"
              }
              spellCheck={isAgent}
              autoComplete="off"
              aria-label={isAgent ? "agent prompt" : "shell command"}
              disabled={frozen}
            />
            {isAgent && pendingRuns > 0 ? (
              <span
                className="agent-queue"
                data-testid="agent-queue"
                role="status"
              >
                {pendingRuns === 1
                  ? "1 submission · checking output"
                  : `${pendingRuns} submissions · checking output`}
              </span>
            ) : null}
            {isAgent ? (
              <button
                type="submit"
                className="agent-send"
                data-testid="ws-prompt-submit"
                disabled={frozen || !cmd.trim()}
              >
                Send
              </button>
            ) : null}
          </form>
        </div>

        <div>
          <div className={"lc-state" + (frozen ? " frozen" : "")}>
            <span className="big" data-testid="ws-state">{sess.state}</span>
          </div>
          {isAgent ? (
            <div className="panel workload-panel" data-testid="ws-workload">
              <h4>Workload</h4>
              <div className="workload-kv">
                <span>Type</span>
                <strong>claude-code</strong>
              </div>
              <div className="workload-kv">
                <span>Model</span>
                <strong>{modelLabel}</strong>
              </div>
              <p>
                Each prompt runs once and exits. Responses remain available
                through the session's offset cursor.
              </p>
            </div>
          ) : null}
          <div className="panel">
            <h4>Actions</h4>
            <div className="action-stack">
              <button
                className="btn btn-ghost"
                data-testid="ws-switch"
                onClick={() => void doSwitch()}
              >
                Switch (AC-C4)
              </button>
            </div>
          </div>
        </div>
      </div>
    </div>
  );
}
