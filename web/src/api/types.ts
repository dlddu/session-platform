// Mirrors control-plane/api/openapi.yaml. Keep in sync with the backend schema.

export type State = "active" | "idle" | "snapshot";

/**
 * AC-E1/AC-G1. 화면은 리터럴이 아니라 계열로 갈린다 — `app/workloadKind.ts`.
 * 승인 게이트는 타입이 아니라 타입의 능력이다(`WorkloadTypeConfig.approvalGate`).
 */
export type WorkloadType =
  | "shell"
  | "claude-code"
  | "minimax-code"
  | "codex-chat"
  | "codex-task";

/**
 * AC-E8 · AC-M4. 값 목록은 타입마다 다르다 — 정본은 `GET /config` 의 `reasoningEfforts`.
 */
export type ReasoningEffort = string;

export interface ModelCatalog {
  defaultModel: string;
  models: string[];
}

export interface WorkloadTypeConfig {
  type: WorkloadType;
  label: string;
  description: string;
  takesModel: boolean;
  input: "prompt" | "terminal";
  /**
   * docs/prd/approval-gate.md AC-F2~F6. 이 타입의 세션이 승인 게이트 뒤에서 도는가.
   * 이 필드가 생기기 전의 control plane 은 보내지 않는다.
   */
  approvalGate?: boolean;
  defaultModel: string | null;
  models: string[];
  /** AC-E8. Empty means the type takes no reasoning effort. */
  reasoningEfforts: ReasoningEffort[];
}

/** AC-K4 */
export interface TenantConfig {
  name: string;
  workloadTypes: WorkloadType[];
}

export interface PlatformConfig {
  claudeCode: ModelCatalog;
  workloadTypes: WorkloadTypeConfig[];
  /** The calling tenant; null for the operator. */
  tenant: TenantConfig | null;
  /** What the operator may create into and filter by; empty for a tenant caller. */
  tenants: TenantConfig[];

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
  /** AC-E8. Absent means the CLI default. */
  reasoningEffort?: ReasoningEffort;
  state: State;
  /** The session's dedicated workload pod — the 1:1 subject of AC-A2. */
  pod?: string;
  /**
   * AC-A2 의 보조 파드 조항 · AC-F4. 워크로드 파드와 수명을 공유하므로 이 필드는
   * `pod` 이 없을 때 정확히 함께 없다.
   */
  auxiliaryPods?: string[];
  /** AC-K2. Absent means operator-owned (untenanted). */
  tenant?: string;
  createdAt: string;
  lastAccess: string;
  checkpoint?: Checkpoint;
}

export interface CreateSessionRequest {
  name: string;
  /** omitted means "shell" (AC-E1) */
  workloadType?: WorkloadType;
  model?: string;
  /** AC-E8. Omitted means the CLI default; only types listing efforts take it. */
  reasoningEffort?: ReasoningEffort;
  /** Operator only (AC-K2); omitted means untenanted. */
  tenant?: string;
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

/** AC-J7 — what a codex-chat or codex-task session's plugin refresh changed. */
export interface PluginRefreshResult {
  session: Session;
  path: string;
  state: "ready" | "unconfigured";
  revision?: string;
  added: string[];
  updated: string[];
  removed: string[];
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
