// Thin client over the control plane REST API. Paths are relative so the same
// code works behind the Vite dev proxy and the embedded prod build.
import type {
  CreateSessionRequest,
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
  return (await res.json()) as T;
}

export const api = {
  listSessions: () =>
    req<{ sessions: Session[] }>("/sessions").then((r) => r.sessions ?? []),

  getSession: (id: string) => req<Session>(`/sessions/${id}`),

  createSession: (body: CreateSessionRequest) =>
    req<Session>("/sessions", { method: "POST", body: JSON.stringify(body) }),

  readSession: (id: string) =>
    req<ReadResult>(`/sessions/${id}/read`, { method: "POST" }),

  writeSession: (id: string, payload: string) =>
    req<WriteResult>(`/sessions/${id}/write`, {
      method: "POST",
      body: JSON.stringify({ payload }),
    }),

  switchSession: (id: string) =>
    req<Session>(`/sessions/${id}/switch`, { method: "POST" }),
};
