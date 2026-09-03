// mockup: docs/mockups/workspace.html, docs/mockups/agent-workspace.html
// docs/mockups/README.md 의 「화면 ↔ mockup 매핑」 표와 양방향으로 일치해야 한다 (scripts/check-render-fidelity.py).
import { useCallback, useEffect, useRef, useState } from "react";
import { Link, useNavigate, useParams } from "react-router-dom";
import { api } from "../api/client";
import type {
  OutputStreamEvent,
  OutputStreamResetEvent,
  OutputStreamState,
  Session,
} from "../api/types";
import { DeleteSessionDialog } from "../app/DeleteSessionDialog";
import { StateBadge } from "../app/StateBadge";
import { useToast } from "../app/Toast";

const SHELL_READ_DELAYS_MS = [250, 700] as const;
const STREAM_RECONNECT_BASE_MS = 250;
const STREAM_RECONNECT_MAX_MS = 5_000;
const STREAM_STATE_LABEL: Record<OutputStreamState, string> = {
  connecting: "output connecting",
  live: "output live",
  reconnecting: "output reconnecting",
  offline: "output offline",
};

function sleep(ms: number): Promise<void> {
  return new Promise((resolve) => setTimeout(resolve, ms));
}

function appendLine(current: string, line: string): string {
  const separator = current === "" || current.endsWith("\n") ? "" : "\n";
  return current + separator + line + "\n";
}

function decodeBase64(payload: string): Uint8Array {
  const binary = atob(payload);
  const bytes = new Uint8Array(binary.length);
  for (let index = 0; index < binary.length; index += 1) {
    bytes[index] = binary.charCodeAt(index);
  }
  return bytes;
}

function isCursor(value: unknown): value is number {
  return (
    typeof value === "number" &&
    Number.isSafeInteger(value) &&
    value >= 0
  );
}

function displayModel(model?: string): string {
  return !model || model === "platform-default" ? "platform default" : model;
}

// Workspace dispatches its interaction model from the immutable workloadType.
// Shell keeps J5's cursor-read loop. Claude Code writes one queued prompt at a
// time while a passive SSE connection appends output bytes as they are emitted.
// POST /read remains an explicit catch-up path, not the normal agent loop.
export function Workspace() {
  const { id = "" } = useParams();
  const [sess, setSess] = useState<Session | null>(null);
  const [error, setError] = useState<string | null>(null);
  const [term, setTerm] = useState("");
  const [cmd, setCmd] = useState("");
  const [pendingSubmissions, setPendingSubmissions] = useState(0);
  const [reading, setReading] = useState(false);
  const [streamState, setStreamState] =
    useState<OutputStreamState>("connecting");
  const [deleteOpen, setDeleteOpen] = useState(false);
  const [archiving, setArchiving] = useState(false);
  const offsetRef = useRef(0);
  const readQueueRef = useRef<Promise<void>>(Promise.resolve());
  const writeQueueRef = useRef<Promise<void>>(Promise.resolve());
  const termRef = useRef<HTMLDivElement>(null);
  const generationRef = useRef(0);
  const stopStreamRef = useRef<(() => void) | null>(null);
  const restartStreamRef = useRef<(() => void) | null>(null);
  const navigate = useNavigate();
  const { toast } = useToast();

  const appendSys = useCallback((line: string) => {
    setTerm((current) => appendLine(current, `◆ ${line}`));
  }, []);

  // POST /read is retained for the shell loop and explicit agent catch-up. A
  // generation guard prevents a late A -> B -> A response from reaching the
  // new workspace, while the byte cursor gate de-duplicates an SSE race.
  const readDelta = useCallback((generation: number): Promise<void> => {
    const request = readQueueRef.current.then(async () => {
      if (generationRef.current !== generation) return;
      setReading(true);
      const requestedOffset = offsetRef.current;
      try {
        const result = await api.readSession(id, requestedOffset);
        if (generationRef.current !== generation) return;
        if (
          !isCursor(result.nextOffset) ||
          result.nextOffset < requestedOffset ||
          new TextEncoder().encode(result.payload).byteLength !==
            result.nextOffset - requestedOffset
        ) {
          throw new Error("invalid output cursor returned by read");
        }
        setSess(result.session);

        const localOffset = offsetRef.current;
        if (result.nextOffset <= localOffset) return;
        // An SSE event advanced while this JSON read was in flight. Its payload
        // cannot be byte-sliced safely, so the live stream remains authoritative.
        if (requestedOffset !== localOffset) return;

        offsetRef.current = result.nextOffset;
        if (result.payload) {
          setTerm((current) => current + result.payload);
        }
      } finally {
        if (generationRef.current === generation) setReading(false);
      }
    });
    readQueueRef.current = request.then(
      () => undefined,
      () => undefined,
    );
    return request;
  }, [id]);

  useEffect(() => {
    const generation = generationRef.current + 1;
    generationRef.current = generation;
    let disposed = false;
    let source: EventSource | null = null;
    let reconnectTimer: number | null = null;
    let reconnectAttempts = 0;
    let transportToken = 0;

    offsetRef.current = 0;
    readQueueRef.current = Promise.resolve();
    writeQueueRef.current = Promise.resolve();
    setTerm("");
    setCmd("");
    setPendingSubmissions(0);
    setArchiving(false);
    setStreamState("connecting");
    setSess(null);
    setError(null);

    const isCurrent = () =>
      !disposed && generationRef.current === generation;

    const closeTransport = () => {
      transportToken += 1;
      if (reconnectTimer !== null) {
        window.clearTimeout(reconnectTimer);
        reconnectTimer = null;
      }
      if (source) {
        source.close();
        source = null;
      }
    };

    const reconnectDelay = () =>
      Math.min(
        STREAM_RECONNECT_BASE_MS * 2 ** Math.min(reconnectAttempts, 5),
        STREAM_RECONNECT_MAX_MS,
      );

    const routeToRestore = (session: Session) => {
      closeTransport();
      setSess(session);
      navigate(`/restore/${id}`, { replace: true });
    };

    function schedule(
      delay: number,
      expectedToken: number,
      task: () => void,
    ) {
      if (!isCurrent() || transportToken !== expectedToken) return;
      if (reconnectTimer !== null) window.clearTimeout(reconnectTimer);
      const timer = window.setTimeout(() => {
        if (reconnectTimer !== timer) return;
        reconnectTimer = null;
        if (!isCurrent() || transportToken !== expectedToken) return;
        task();
      }, delay);
      reconnectTimer = timer;
    }

    function scheduleConnect(delay: number, expectedToken: number) {
      schedule(delay, expectedToken, connect);
    }

    function reconnectFromCursor(reason: string) {
      if (!isCurrent()) return;
      closeTransport();
      const reconnectToken = transportToken;
      appendSys(`output stream reset: ${reason}`);
      setStreamState("reconnecting");
      const delay = reconnectDelay();
      reconnectAttempts += 1;
      scheduleConnect(delay, reconnectToken);
    }

    async function recoverAfterError(expectedToken: number) {
      if (!isCurrent() || transportToken !== expectedToken) return;
      try {
        const session = await api.getSession(id);
        if (!isCurrent() || transportToken !== expectedToken) return;
        setSess(session);
        if (session.state === "snapshot") {
          routeToRestore(session);
          return;
        }

        setStreamState("reconnecting");
        const delay = reconnectDelay();
        reconnectAttempts += 1;
        scheduleConnect(delay, expectedToken);
      } catch {
        if (!isCurrent() || transportToken !== expectedToken) return;
        setStreamState("offline");
        const delay = reconnectDelay();
        reconnectAttempts += 1;
        schedule(delay, expectedToken, () => {
          void recoverAfterError(expectedToken);
        });
      }
    }

    async function reconcileAfterReset(expectedToken: number) {
      if (!isCurrent() || transportToken !== expectedToken) return;
      const request = readQueueRef.current.then(async () => {
        if (!isCurrent() || transportToken !== expectedToken) return null;
        setReading(true);
        const result = await api.readSession(id, 0);
        if (!isCurrent() || transportToken !== expectedToken) return null;
        if (
          !isCursor(result.nextOffset) ||
          new TextEncoder().encode(result.payload).byteLength !==
            result.nextOffset
        ) {
          throw new Error("invalid full output returned by reset read");
        }
        return result;
      });
      readQueueRef.current = request.then(
        () => undefined,
        () => undefined,
      );

      try {
        const result = await request;
        if (
          result === null ||
          !isCurrent() ||
          transportToken !== expectedToken
        ) {
          return;
        }

        setSess(result.session);
        if (result.session.state === "snapshot") {
          routeToRestore(result.session);
          return;
        }

        // A reset means retained history diverged. Replace mixed local/server
        // scrollback with the server's authoritative replay before reconnecting.
        offsetRef.current = result.nextOffset;
        setTerm(result.payload);
        reconnectAttempts = 0;
        connect();
      } catch (resetError) {
        if (!isCurrent() || transportToken !== expectedToken) return;
        appendSys(`output reset recovery failed: ${resetError}`);
        setStreamState("offline");
        const delay = reconnectDelay();
        reconnectAttempts += 1;
        schedule(delay, expectedToken, () => {
          void recoverAfterError(expectedToken);
        });
      } finally {
        if (generationRef.current === generation) setReading(false);
      }
    }

    function connect() {
      if (!isCurrent()) return;
      closeTransport();
      const candidateToken = transportToken;
      setStreamState(reconnectAttempts === 0 ? "connecting" : "reconnecting");

      const candidate = api.streamSession(id, offsetRef.current);
      source = candidate;
      const isCurrentCandidate = () =>
        isCurrent() &&
        transportToken === candidateToken &&
        source === candidate;

      candidate.onopen = () => {
        if (!isCurrentCandidate()) return;
        setStreamState("live");
      };
      candidate.addEventListener("reset", (rawEvent) => {
        if (!isCurrentCandidate()) return;
        const message = rawEvent as MessageEvent<string>;
        try {
          const event = JSON.parse(message.data) as OutputStreamResetEvent;
          if (!isCursor(event.nextOffset)) {
            throw new Error("invalid reset cursor");
          }
          if (
            message.lastEventId === "" ||
            message.lastEventId !== String(event.nextOffset)
          ) {
            throw new Error("reset event id does not match nextOffset");
          }
        } catch (resetError) {
          reconnectFromCursor(`invalid reset event: ${resetError}`);
          return;
        }

        // The server says retained history is shorter than our local cursor.
        // Stop this source before its post-reset events, then replace the full
        // rendered history from JSON /read and reconnect at that read cursor.
        closeTransport();
        const resetToken = transportToken;
        setStreamState("reconnecting");
        void reconcileAfterReset(resetToken);
      });
      candidate.addEventListener("output", (rawEvent) => {
        if (!isCurrentCandidate()) return;
        const message = rawEvent as MessageEvent<string>;
        let event: OutputStreamEvent;
        let bytes: Uint8Array;
        try {
          event = JSON.parse(message.data) as OutputStreamEvent;
          if (
            !isCursor(event.offset) ||
            !isCursor(event.nextOffset) ||
            event.nextOffset < event.offset ||
            typeof event.payloadBase64 !== "string"
          ) {
            throw new Error("invalid cursor fields");
          }
          if (
            message.lastEventId === "" ||
            message.lastEventId !== String(event.nextOffset)
          ) {
            throw new Error("event id does not match nextOffset");
          }
          bytes = decodeBase64(event.payloadBase64);
          if (bytes.byteLength !== event.nextOffset - event.offset) {
            throw new Error("payload byte length does not match cursor range");
          }
        } catch (decodeError) {
          reconnectFromCursor(String(decodeError));
          return;
        }

        const localOffset = offsetRef.current;
        if (event.nextOffset <= localOffset) return;
        if (event.offset > localOffset) {
          reconnectFromCursor(
            `cursor gap ${localOffset}..${event.offset}`,
          );
          return;
        }

        const unseen = bytes.subarray(localOffset - event.offset);
        let text: string;
        try {
          // Every issued cursor is a UTF-8 boundary, including localOffset
          // after overlap slicing. One-shot fatal decode makes violations fail
          // before the byte cursor advances and leaves no pending decoder state
          // for a concurrent JSON /read catch-up.
          text = new TextDecoder("utf-8", { fatal: true }).decode(unseen);
        } catch (decodeError) {
          reconnectFromCursor(`invalid UTF-8 output: ${decodeError}`);
          return;
        }

        offsetRef.current = event.nextOffset;
        if (text) setTerm((current) => current + text);
        reconnectAttempts = 0;
        setStreamState("live");
      });
      candidate.onerror = () => {
        if (!isCurrentCandidate()) return;
        closeTransport();
        const recoveryToken = transportToken;
        setStreamState("reconnecting");
        void recoverAfterError(recoveryToken);
      };
    }

    const restartStream = () => {
      if (!isCurrent()) return;
      closeTransport();
      reconnectAttempts = 0;
      connect();
    };
    stopStreamRef.current = closeTransport;
    restartStreamRef.current = restartStream;

    void api
      .getSession(id)
      .then(async (session) => {
        if (!isCurrent()) return;
        setSess(session);
        if (session.state === "snapshot") {
          routeToRestore(session);
          return;
        }
        if (session.workloadType === "claude-code") {
          connect();
          return;
        }
        setStreamState("offline");
        try {
          await readDelta(generation);
        } catch (readError) {
          if (isCurrent()) appendSys(`read failed: ${readError}`);
        }
      })
      .catch((loadError) => {
        if (isCurrent()) setError(String(loadError));
      });

    return () => {
      disposed = true;
      if (generationRef.current === generation) {
        generationRef.current += 1;
      }
      closeTransport();
      if (stopStreamRef.current === closeTransport) {
        stopStreamRef.current = null;
      }
      if (restartStreamRef.current === restartStream) {
        restartStreamRef.current = null;
      }
    };
  }, [appendSys, id, navigate, readDelta]);

  useEffect(() => {
    const element = termRef.current;
    if (element) element.scrollTop = element.scrollHeight;
  }, [term]);

  async function refreshOutput() {
    const generation = generationRef.current;
    try {
      await readDelta(generation);
    } catch (readError) {
      if (generationRef.current === generation) {
        appendSys(`read failed: ${readError}`);
      }
    }
  }

  async function run() {
    const command = cmd;
    const isAgent = sess?.workloadType === "claude-code";
    const generation = generationRef.current;
    setCmd("");

    if (command.trim() === "") {
      await refreshOutput();
      return;
    }

    if (isAgent) {
      setTerm((current) => appendLine(current, `▸ ${command}`));
      setPendingSubmissions((count) => count + 1);

      // Serialize acceptance requests as well as the server-side executions so
      // two rapidly submitted prompts cannot arrive out of user-visible order.
      const submission = writeQueueRef.current.then(async () => {
        if (generationRef.current !== generation) return;
        const result = await api.writeSession(id, command);
        if (generationRef.current === generation) setSess(result.session);
      });
      writeQueueRef.current = submission.then(
        () => undefined,
        () => undefined,
      );

      try {
        await submission;
      } catch (submissionError) {
        if (generationRef.current === generation) {
          appendSys(`agent submission failed; not retried: ${submissionError}`);
        }
      } finally {
        if (generationRef.current === generation) {
          setPendingSubmissions((count) => Math.max(0, count - 1));
        }
      }
      return;
    }

    try {
      const result = await api.writeSession(id, command + "\n");
      if (generationRef.current !== generation) return;
      setSess(result.session);
      await readDelta(generation);
      for (const delay of SHELL_READ_DELAYS_MS) {
        await sleep(delay);
        await readDelta(generation);
      }
    } catch (ioError) {
      if (generationRef.current === generation) {
        appendSys(`shell i/o failed: ${ioError}`);
      }
    }
  }

  async function doSwitch() {
    const generation = generationRef.current;
    try {
      const active = await api.switchSession(id);
      if (generationRef.current !== generation) return;
      setSess(active);
      if (active.workloadType === "claude-code") restartStreamRef.current?.();
      appendSys(`switch → ${active.state}`);
    } catch (switchError) {
      if (generationRef.current === generation) {
        appendSys(`switch failed: ${switchError}`);
      }
    }
  }

  async function archiveNow() {
    if (!sess || sess.state === "snapshot" || archiving) return;

    const generation = generationRef.current;
    const isAgent = sess.workloadType === "claude-code";
    const action = isAgent ? "archive" : "freeze";

    setArchiving(true);
    stopStreamRef.current?.();
    if (isAgent) setStreamState("offline");

    try {
      const archived = await api.archiveSession(id);
      if (generationRef.current !== generation) return;
      if (archived.state !== "snapshot") {
        throw new Error(
          `archive returned unexpected session state "${archived.state}"`,
        );
      }

      setSess(archived);
      toast(
        isAgent
          ? "Session archived — pod reclaimed"
          : "Session frozen — pod reclaimed",
        "frozen",
      );
      navigate("/", { replace: true });
    } catch (archiveError) {
      if (generationRef.current !== generation) return;

      setArchiving(false);
      appendSys(`${action} failed: ${archiveError}`);
      toast(`Could not ${action} session: ${archiveError}`, "warm");
      if (isAgent) restartStreamRef.current?.();
    }
  }

  function handleDeleted(deleted: Session) {
    stopStreamRef.current?.();
    toast('Session "' + deleted.name + '" deleted');
    navigate("/", { replace: true });
  }

  if (error) return <div className="pad error">Failed to load session: {error}</div>;
  if (!sess) return <div className="pad empty">Loading…</div>;

  const isAgent = sess.workloadType === "claude-code";
  const frozen = sess.state === "snapshot";
  const modelLabel = displayModel(sess.model?.trim());
  const archiveButtonLabel = archiving
    ? (isAgent ? "Archiving…" : "Freezing…")
    : (isAgent ? "Archive now" : "Freeze now");

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
                isAgent ? (
                  <span
                    className={`tag stream-status ${streamState}`}
                    data-testid="ws-stream-status"
                    data-stream-state={streamState}
                    role="status"
                  >
                    <span className="led" />
                    {STREAM_STATE_LABEL[streamState]}
                  </span>
                ) : (
                  <span className="tag">
                    <span className="led" />
                    shell attached
                  </span>
                )
              ) : null}
              <button
                type="button"
                className="console-refresh"
                data-testid="ws-refresh-output"
                onClick={() => void refreshOutput()}
                disabled={reading || archiving}
                title={
                  isAgent
                    ? "Fallback cursor read if the live stream is interrupted"
                    : undefined
                }
              >
                {reading
                  ? isAgent ? "Catching up…" : "Refreshing…"
                  : isAgent ? "Catch up" : "Refresh output"}
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
                  ? "// ready — agent responses stream here automatically"
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
              disabled={frozen || archiving}
            />
            {isAgent && pendingSubmissions > 0 ? (
              <span
                className="agent-queue"
                data-testid="agent-queue"
                role="status"
              >
                {pendingSubmissions === 1
                  ? "1 submission pending"
                  : `${pendingSubmissions} submissions pending`}
              </span>
            ) : null}
            {isAgent ? (
              <button
                type="submit"
                className="agent-send"
                data-testid="ws-prompt-submit"
                disabled={frozen || archiving || !cmd.trim()}
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
                Each prompt runs once and exits. Responses stream live and
                remain replayable through the session's offset cursor.
              </p>
            </div>
          ) : null}
          <div className="panel">
            <h4>Actions</h4>
            <div className="action-stack">
              <button
                className="btn btn-ghost"
                type="button"
                data-testid="ws-archive-session"
                onClick={() => void archiveNow()}
                disabled={
                  frozen ||
                  archiving ||
                  reading ||
                  pendingSubmissions > 0
                }
                aria-busy={archiving}
              >
                <svg
                  viewBox="0 0 24 24"
                  fill="none"
                  stroke="currentColor"
                  strokeWidth="2"
                  strokeLinecap="round"
                  strokeLinejoin="round"
                  aria-hidden="true"
                >
                  <rect x="3" y="4" width="18" height="5" rx="1.5" />
                  <path d="M5 9v10a1.5 1.5 0 0 0 1.5 1.5h11A1.5 1.5 0 0 0 19 19V9M10 13h4" />
                </svg>
                {archiveButtonLabel}
              </button>
              <button
                className="btn btn-ghost"
                data-testid="ws-switch"
                onClick={() => void doSwitch()}
                disabled={archiving}
              >
                Switch (AC-C4)
              </button>
              <button
                className="btn btn-danger"
                type="button"
                data-testid="ws-delete-session"
                onClick={() => setDeleteOpen(true)}
                disabled={archiving}
              >
                Delete session
              </button>
            </div>
          </div>
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
