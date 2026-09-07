// Mirrors control-plane/api/openapi.yaml. Keep in sync with the backend schema.

export type State = "active" | "idle" | "snapshot";

/**
 * AC-E1/AC-F1. 화면은 리터럴이 아니라 계열로 갈린다 — `app/workloadKind.ts`.
 */
export type WorkloadType = "shell" | "claude-code" | "approval-gated";

export interface PlatformConfig {
  claudeCode: {
    defaultModel: string;
    models: string[];
  };
}

export interface Checkpoint {
  ref: string;
  sizeBytes: number;
  createdAt: string;
  reclaimed?: string;
}

export interface Session {
  id: string;
  name: string;
  workloadType: WorkloadType;
  model?: string;
  state: State;
  /** The session's dedicated workload pod — the 1:1 subject of AC-A2. */
  pod?: string;
  /**
   * AC-A2 의 보조 파드 조항 · AC-F4. 워크로드 파드와 수명을 공유하므로 이 필드는
   * `pod` 이 없을 때 정확히 함께 없다.
   */
  auxiliaryPods?: string[];
  createdAt: string;
  lastAccess: string;
  checkpoint?: Checkpoint;
}

export interface CreateSessionRequest {
  name: string;
  /** omitted means "shell" (AC-E1) */
  workloadType?: WorkloadType;
  model?: string;
}

export interface ReadResult {
  session: Session;
  path: string;
  /** AC-D3/AC-E3 */
  payload: string;
  nextOffset: number;
}

/** 세션 SSE 스트림의 `output` 이벤트 — 커서·base64 규약은 AC-E3. */
export interface OutputStreamEvent {
  offset: number;
  payloadBase64: string;
  nextOffset: number;
}

/** The server's authoritative cursor after retained output history changed. */
export interface OutputStreamResetEvent {
  nextOffset: number;
}

export type OutputStreamState =
  | "connecting"
  | "live"
  | "reconnecting"
  | "offline";

export interface WriteResult {
  session: Session;
  path: string;
}
