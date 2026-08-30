// Mirrors control-plane/api/openapi.yaml. Keep in sync with the backend schema.

export type State = "active" | "idle" | "snapshot";

/**
 * Which data plane workload the session runs (AC-E1). Chosen at creation and
 * immutable afterwards. NewSession offers shell (the default) and claude-code.
 */
export type WorkloadType = "shell" | "claude-code";

export interface PlatformConfig {
  claudeCode: {
    /** Concrete Secret default when configured; otherwise platform-default. */
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
  /** Claude model selected at creation; omitted means the platform default. */
  model?: string;
  state: State;
  pod?: string;
  createdAt: string;
  lastAccess: string;
  checkpoint?: Checkpoint;
}

export interface CreateSessionRequest {
  name: string;
  /** omitted means "shell" (AC-E1) */
  workloadType?: WorkloadType;
  /** Only applies to claude-code. Omitted means the platform default (AC-E6). */
  model?: string;
}

export interface ReadResult {
  session: Session;
  path: string;
  /** workload output accumulated after the requested offset (AC-D3/AC-E3) */
  payload: string;
  /** cursor to pass as offset on the next read to receive only new output */
  nextOffset: number;
}

/**
 * One `output` event from the session SSE stream. Payload bytes are base64 so
 * byte offsets, overlap slicing, and wire lengths stay exact. Claude output
 * event cursors are always issued at UTF-8 code-point boundaries.
 */
export interface OutputStreamEvent {
  /** byte offset of the first payload byte */
  offset: number;
  payloadBase64: string;
  /** byte offset immediately after the last payload byte */
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
