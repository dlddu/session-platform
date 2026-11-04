// Mirrors control-plane/api/openapi.yaml. Keep in sync with the backend schema.

export type State = "active" | "idle" | "snapshot";

/**
 * AC-E1/AC-G1. 화면은 리터럴이 아니라 계열로 갈린다 — `app/workloadKind.ts`.
 * 승인 게이트는 타입이 아니라 타입의 능력이다(`WorkloadTypeConfig.approvalGate`).
 */
export type WorkloadType = "shell" | "claude-code" | "minimax-code";

/** GET /config 의 workloadTypes 한 항목 중 화면이 읽는 필드. */
export interface WorkloadTypeConfig {
  type: string;
  /** docs/prd/approval-gate.md AC-F2~F6. 이 타입의 세션이 승인 게이트 뒤에서 도는가. */
  approvalGate?: boolean;
}

export interface PlatformConfig {
  claudeCode: {
    defaultModel: string;
    models: string[];
  };
  /** 오래된 control plane 은 이 필드가 없다. */
  workloadTypes?: WorkloadTypeConfig[];
}

// 와이어 필드 이름은 `checkpoint` 그대로다(OpenAPI 계약). 가리키는 것은 모든 타입에서 파일시스템 아카이브다.
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

export type AllowlistSource = "manual" | "request";

export interface AllowlistEntry {
  repo: string;
  source: AllowlistSource;
  requestSessionId?: string;
  addedAt: string;
}

export interface AccessRequest {
  id: string;
  repo: string;
  sessionId: string;
  sessionName?: string;
  workloadType?: string;
  reason: string;
  requestedAt: string;
  alreadyAllowed: boolean;
}

export interface RepoAccessState {
  allowlist: AllowlistEntry[];
  requests: AccessRequest[];
}
