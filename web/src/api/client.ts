// Thin client over the control plane REST API. Paths are relative so the same
// code works behind the Vite dev proxy and the embedded prod build.
import type {
  AllowlistEntry,
  CreateSessionRequest,
  PlatformConfig,
  PendingQuestion,
  PluginRefreshResult,
  ReadResult,
  RepoAccessState,
  Session,
  TenantConfig,
  WorkloadType,
  WorkloadTypeConfig,
  WriteResult,
} from "./types";

// AC-J1: `codex` 는 `codex-chat` 의 옛 이름이다. 서버는 늘 새 이름을 내지만, 이름 변경
// 전의 제어면과 섞여 돌 때도 화면이 한 갈래로 읽게 여기서 한 번 접는다.
function normalizeType(t: WorkloadType | "codex"): WorkloadType {
  return t === "codex" ? "codex-chat" : t;
}

function normalizeSession<T extends Session | undefined>(s: T): T {
  return s ? { ...s, workloadType: normalizeType(s.workloadType) } : s;
}

function withSession<T extends { session: Session }>(r: T): T {
  return { ...r, session: normalizeSession(r.session) };
}

function normalizeTenant(t: TenantConfig): TenantConfig {
  return { ...t, workloadTypes: (t.workloadTypes ?? []).map(normalizeType) };
}

function normalizeTypeConfig(t: WorkloadTypeConfig): WorkloadTypeConfig {
  return {
    ...t,
    type: normalizeType(t.type),
    reasoningEfforts: t.reasoningEfforts ?? [],
  };
}

const BASE = "/api/v1";

export class ApiError extends Error {
  readonly status: number;
  readonly detail: string;

  constructor(status: number, detail: string) {
    super(`${status} ${detail}`);
    this.status = status;
    this.detail = detail;
  }
}

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
    throw new ApiError(res.status, detail);
  }
  if (res.status === 204) return undefined as T;
  return (await res.json()) as T;
}

export const api = {
  // Older control planes omit the tenancy and catalog fields.
  getConfig: () =>
    req<Partial<PlatformConfig> & Pick<PlatformConfig, "claudeCode">>("/config", {
      cache: "no-store",
    }).then(
      (r): PlatformConfig => ({
        claudeCode: r.claudeCode,
        workloadTypes: (r.workloadTypes ?? []).map(normalizeTypeConfig),
        tenant: r.tenant ? normalizeTenant(r.tenant) : null,
        tenants: (r.tenants ?? []).map(normalizeTenant),
      }),
    ),

  listSessions: () =>
    req<{ sessions: Session[] }>("/sessions").then((r) =>
      (r.sessions ?? []).map(normalizeSession),
    ),

  getSession: (id: string) =>
    req<Session>(`/sessions/${id}`).then(normalizeSession),

  createSession: (body: CreateSessionRequest) =>
    req<Session>("/sessions", { method: "POST", body: JSON.stringify(body) }).then(
      normalizeSession,
    ),

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
    }).then(withSession),

  writeSession: (id: string, payload: string) =>
    req<WriteResult>(`/sessions/${id}/write`, {
      method: "POST",
      body: JSON.stringify({ payload }),
    }).then(withSession),

  switchSession: (id: string) =>
    req<Session>(`/sessions/${id}/switch`, { method: "POST" }).then(normalizeSession),

  /** Archive immediately and reclaim the workload pod (every workload type). */
  archiveSession: (id: string) =>
    req<Session>(`/sessions/${id}/snapshot`, { method: "POST" }).then(normalizeSession),

  /** AC-J7 — codex-chat and codex-task only, between turns; the conversation is kept. */
  refreshPlugins: (id: string) =>
    req<PluginRefreshResult>(`/sessions/${id}/plugins/refresh`, { method: "POST" }).then(
      withSession,
    ),

  /** AC-E2 — claude-code only: what the running turn asked a person. */
  listQuestions: (id: string) =>
    req<{ questions: PendingQuestion[] }>(`/sessions/${id}/questions`, {
      cache: "no-store",
    }).then((r) => r.questions ?? []),

  /** AC-E2 — one answer per question, keyed by the question's text. */
  answerQuestion: (id: string, questionId: string, answers: Record<string, string>) =>
    req<{ status: string }>(
      `/sessions/${id}/questions/${encodeURIComponent(questionId)}/answer`,
      { method: "POST", body: JSON.stringify({ answers }) },
    ),

  getRepoAccess: () =>
    req<RepoAccessState>("/repo-access", { cache: "no-store" }).then((r) => ({
      allowlist: r.allowlist ?? [],
      requests: r.requests ?? [],
    })),

  addAllowlist: (repo: string) =>
    req<AllowlistEntry>("/repo-access/allowlist", {
      method: "POST",
      body: JSON.stringify({ repo }),
    }),

  removeAllowlist: (repo: string) =>
    req<void>(`/repo-access/allowlist?repo=${encodeURIComponent(repo)}`, {
      method: "DELETE",
    }),

  approveRequest: (id: string) =>
    req<AllowlistEntry>(`/repo-access/requests/${encodeURIComponent(id)}/approve`, {
      method: "POST",
    }),

  rejectRequest: (id: string) =>
    req<void>(`/repo-access/requests/${encodeURIComponent(id)}/reject`, {
      method: "POST",
    }),
};
