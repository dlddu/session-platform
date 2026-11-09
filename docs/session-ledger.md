# Session task ledger

The `db` MCP on the codex-chat loopback sidecar offers `ledger_get`,
`ledger_list`, `ledger_put`, and `ledger_delete` alongside the raw SQL tools.
This is the chat slot's bookkeeping, not a global task service or authoritative
platform session inventory. No tool calls session create/delete, sends push,
starts turns, or schedules work. codex-task and Claude workers do not receive
this DB. The parent collects their final result and records it on return;
never pass a parent's token or DB access to a worker.

## Storage

The codex-chat slot PVC mounts `auth/` into the workload and `db/` into the
sidecar at `/var/run/session-platform/session-db`. Only slot-init mounts the
whole PVC. The database belongs to the **slot**, not to a session: a chat
session that is deleted and recreated, and lands on the same slot, opens the
same `session.db` and continues the same ledger (AC-N6). The workload gets a
loopback endpoint, not a file mount; the existing pod/network boundary is the
authorization boundary. Ledger `owner` is a bookkeeping field, not an
authorization claim, and records written by an earlier session on the slot
keep that session's ID as owner. Raw SQL can modify ledger tables: these APIs
are not a security boundary against the same slot.

SQLite uses DELETE journaling (NFS/EFS cannot use WAL shared memory), a 5s busy
timeout and one pooled connection. Each ledger call has a 30s deadline and a
transaction; same-sidecar concurrent writers serialize, and expected_version
selects one winner. External SQLite writers are unsupported. Transactions
close/roll back on error and task mutations and idempotency receipts commit
together. Raw SQL now rolls back any still-open transaction at call end,
including after a statement failure. Use BEGIN/COMMIT within one raw_exec call;
transactions cannot span MCP calls. Autocommit statements before a later
multi-statement error can already be committed. raw_exec counters are SQLite
connection counters, **not committed totals**: ROLLBACK and DDL can report a
prior rowsAffected/lastInsertId. Query actual state when verifying raw writes.

codex-chat retains its slot lease while snapshotted, including recovery and failed restore; it is not eligible for stale frozen-holder reclaim. Explicit deletion releases it. Consequently frozen chats still consume pool capacity. Other workload types continue releasing login slots on snapshot.

DB data is outside the workload archive. Restart, restore and recreation on
the same slot reopen the same PVC path, not a snapshot of the DB. PVC loss or
moving to another slot (a pool with more than one codex-chat slot) is not
covered. Explicit session deletion releases the slot and keeps `db/` for the
next session on it; deleting data is an operator concern, never implicit
ledger CRUD.

**History:** PR #216 briefly scoped the directory to `db/<sessionID>/`, so a
recreated chat opened an empty DB. That was reverted to the slot-level `db/`.
Directories `db/<sessionID>/` left from that window are retained, not merged
automatically. Moving one into `db/session.db` is an offline operation: the
source session must be gone (no sidecar has the file open), copy with the
SQLite backup API, check integrity and row counts, and keep the replaced file
as a backup.

## API

IDs/request IDs use 1–128 ASCII letters, digits, `_` or `-`. Caller-generated
stable IDs let an ambiguous first response be retried. Deleted task IDs are
retired permanently to prevent stale version/ABA updates.

| Tool | Arguments | Result |
| --- | --- | --- |
| ledger_get | id | {id, version, task} |
| ledger_list | status?, stage?, owner?, session_id?, after_id?, limit? | {tasks, next_cursor} |
| ledger_put | id, request_id, expected_version, task | {id, version, task} |
| ledger_delete | id, request_id, expected_version | {id, version, deleted:true} |

put is full replacement. expected_version=0 creates; positive versions must
match the current record atomically. Get before updating and preserve unrelated
fields. Versions/cursors use safe JSON integers. A mutation's request_id is
unique across this DB: identical tool+arguments returns the original committed
response even after subsequent updates/deletion; changed arguments conflict.
Get again to learn current state after a replay. Failed mutations do not reserve
request IDs. Delete removes only the ledger task; idempotency responses and
retired IDs remain. This is not secure erasure. Receipts currently have no TTL;
keep records compact and monitor the slot's storage budget.

list orders by ID, defaults to 20, caps at 100 and approximately 512KiB of task
JSON. Pass next_cursor unchanged as after_id; empty means done. Filters combine
with AND. Pages are not a snapshot: concurrent inserts before the cursor can
be missed; refresh for reconciliation before deletion. next_cursor is separate
from session_read's byte nextOffset.

Tool errors use isError with code invalid_argument, not_found, conflict,
schema_version, or storage_error. A missing update/delete target is not_found;
a stale version, reused ID or changed request payload is conflict. Unsupported
schema versions/collisions fail closed. Storage errors do not expose SQL or
payload. Reconcile ambiguous commit outcomes by retrying identical arguments.

A task has title (1–512 bytes), status
(planned/running/blocked/review/completed/unknown), stage (1–128 bytes), owner,
sessions, and details. Total task JSON is at most 64KiB; at most 100 related
sessions. Each relation has id, ownership (owned/reused/unknown), purpose,
nextOffset, optional seq/end_seq, cleanup (pending/retained/deleting/deleted),
and optional reason/next_action. Retention requires reason/next_action;
only owned relations may be recorded deleting/deleted. The API does not
validate real-world ownership, completion, preservation or deletion evidence.

Bounded details JSON carries approval scope/restrictions; result summaries and
PR/head links; validation and provenance/observed_at; notification assignee,
state and sent/devices; preservation links/state; external operation intent
and next action. Never store transcripts, credentials or login codes. Historical
information starts unknown with source/time; it is not imported as running.

Schema v1 creates only reserved `sp_ledger_*` tables transactionally on first
successful call. It does not change PRAGMA user_version or existing user tables.
A namespace collision or future version is refused without destructive repair.
Any future version needs an explicit transactional migration; never DROP user
tables or silently downgrade. Do not mutate the reserved tables using raw SQL.

## External operation reconciliation

Before create/write/delete, record operation key/kind/intent and approved scope.
The key is bookkeeping, not platform idempotency. After success, record actual
ID/response. If DB recording fails, preserve that response in the conversation;
do not repeat the external operation. A write acceptance ends the parent's turn
immediately, even if its receipt needs reconciliation.

On user return, read only the owned/authorized session at its saved nextOffset,
store the exact returned cursor, and require final plus matching end seq.
Worker push acceptance is not result collection or evidence of user readership.
For unknown create outcomes reconcile inventory/creation evidence before any
retry; names are not unique. For delete-response loss check passive inventory,
not session_read (which can restore a pod). Record deletion only after confirmed
absence, retaining unresolved state/reason/next action. Never delete while
unpreserved/unpushed work or a queued/running turn remains. Keep review sessions
only for actual follow-up. Merge requires an explicit request; skill refresh
and list/read verification follow a confirmed merge before parent cleanup.

## Verification and rollout boundary

Local tests exercise preservation of user tables/user_version, schema refusal,
transaction rollback on receipt failure, single-winner CAS, replay and ID reuse,
validation, filtering/keyset pages, raw transaction cleanup and close/reopen.
Pod-spec tests cover the slot-level mount. These do not claim a deployed NFS restart/snapshot/restore test.

After authorized merge/deployment, verify tool discovery in the parent,
restart and snapshot/restore with the same slot, and that a deleted and
recreated chat session on the same slot reads the earlier session's rows.
Refresh parent skills, then list/read changed skills and references. A skill
refresh alone does not deploy runtime tools; before availability use the skill's
conversation-record fallback. This implementation session does not create test
cluster sessions, merge, deploy, or refresh the parent.
