// Thin client over the control plane REST API. Paths are relative so the same
// code works behind the Vite dev proxy and the embedded prod build.
import type {
  CreateSessionRequest,
  PlatformConfig,
  ReadResult,
  Session,
  WriteResult,
} from "./types";

const BASE = "/api/v1";

async function req<T>(path: string, init?: RequestInit): Promise<T> {
  const res = await fetch(BASE + path, {
    headers: { "Content-Type": "application/json" },
    ...init,
  });
  if (!res.ok) {
    let detail = res.statusText;
    try {
      const body = (await res.json()) as { error?: string };
      if (body.error) detail = body.error;
    } catch {
      /* ignore non-JSON error bodies */
    }
    throw new Error(`${res.status} ${detail}`);
  }
  if (res.status === 204) return undefined as T;
  return (await res.json()) as T;
}

export const api = {
  getConfig: () => req<PlatformConfig>("/config", { cache: "no-store" }),

  listSessions: () =>
    req<{ sessions: Session[] }>("/sessions").then((r) => r.sessions ?? []),

  getSession: (id: string) => req<Session>(`/sessions/${id}`),

  createSession: (body: CreateSessionRequest) =>
    req<Session>("/sessions", { method: "POST", body: JSON.stringify(body) }),

  deleteSession: (id: string) =>
    req<void>(`/sessions/${id}`, { method: "DELETE" }),

  /**
   * 재연결 정책은 호출자의 몫이고, 돌려준 EventSource 는 호출자가 닫아야 한다.
   */
  streamSession: (id: string, offset = 0) =>
    new EventSource(
      `${BASE}/sessions/${encodeURIComponent(id)}/stream?offset=${encodeURIComponent(String(offset))}`,
    ),

  /** offset: 직전 read 가 발급한 nextOffset 커서 (AC-D3/AC-E3). */
  readSession: (id: string, offset = 0) =>
    req<ReadResult>(`/sessions/${id}/read`, {
      method: "POST",
      body: JSON.stringify({ offset }),
    }),

  writeSession: (id: string, payload: string) =>
    req<WriteResult>(`/sessions/${id}/write`, {
      method: "POST",
      body: JSON.stringify({ payload }),
    }),

  switchSession: (id: string) =>
    req<Session>(`/sessions/${id}/switch`, { method: "POST" }),

  /** Archive/checkpoint immediately and reclaim the workload pod. */
  archiveSession: (id: string) =>
    req<Session>(`/sessions/${id}/snapshot`, { method: "POST" }),
};
