// Mirrors control-plane/api/openapi.yaml. Keep in sync with the backend schema.

export type State = "active" | "idle" | "snapshot";

/**
 * Which data plane workload the session runs (AC-E1). Chosen at creation and
 * immutable afterwards. The UI does not offer the choice yet — the type
 * selector in docs/mockups/new-session.html is still ahead of the code — so
 * every session the SPA creates is a shell session.
 */
export type WorkloadType = "shell" | "claude-code";

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
}

export interface ReadResult {
  session: Session;
  path: string;
  /** shell output accumulated after the requested offset (AC-D3) */
  payload: string;
  /** cursor to pass as offset on the next read to receive only new output */
  nextOffset: number;
}

export interface WriteResult {
  session: Session;
  path: string;
}
