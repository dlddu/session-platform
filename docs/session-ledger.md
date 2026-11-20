# Session task ledger

The `db` MCP on the codex-chat loopback sidecar offers `ledger_get`,
`ledger_list`, `ledger_put`, and `ledger_delete`. These four are the whole
default surface: `raw_query` and `raw_exec` are debug tools, left out of
`tools/list` and refused in `tools/call` unless the deployment opted that
sidecar in with `SESSION_DB_RAW_SQL=debug` (ConfigMap key
`session-db-raw-sql`). The ledger API is unchanged either way.
This is the chat slot's bookkeeping, not a global task service or authoritative
platform session inventory. No tool calls session create/delete, sends push,
starts turns, or schedules work. codex-task and Claude workers do not receive
this DB. The parent collects their final result and records it on return;
never pass a parent's token or DB access to a worker.

The same `db` MCP also offers the **work records** — `task_*` and `turn_*` —
which are the shape the repository owner agreed on and the one new work is
written in. The ledger stays: it is where a migrated slot's parent bookkeeping
and task history keep living, and it is what an older image still reads. See
[Work records](#work-records-tasks-and-turns).

## Storage

The codex-chat slot PVC mounts `auth/` into the workload and `db/` into the
sidecar at `/var/run/session-platform/session-db`. Only slot-init mounts the
whole PVC. The database belongs to the **slot**, not to a session: a chat
session that is deleted and recreated, and lands on the same slot, opens the
same `session.db` and continues the same ledger (AC-N6). The workload gets a
loopback endpoint, not a file mount; the existing pod/network boundary is the
authorization boundary. Ledger `owner` is a bookkeeping field, not an
authorization claim, and records written by an earlier session on the slot
keep that session's ID as owner. Raw SQL, where a debug sidecar offers it, can
modify ledger tables: these APIs are not a security boundary against the same
slot.

SQLite uses DELETE journaling (NFS/EFS cannot use WAL shared memory), a 5s busy
timeout and one pooled connection. Each ledger call has a 30s deadline and a
transaction; same-sidecar concurrent writers serialize, and expected_version
selects one winner. External SQLite writers are unsupported. Transactions
close/roll back on error and task mutations and idempotency receipts commit
together. Raw SQL — on a debug sidecar — rolls back any still-open transaction
at call end, including after a statement failure. Use BEGIN/COMMIT within one raw_exec call;
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

`details.kind` names what a row is. The ledger schema has no column for it, so
it is a convention in `details`, and the one value readers outside the session
must honour is **`monitor`**: a row the parent's own monitoring loop keeps for
bookkeeping. Monitor rows are not the user's work, so a reader that presents the
ledger to a person (the app's task panel) leaves them out. A row with no
`details.kind` is ordinary work and is shown.

## Read-only HTTP view

Outside the session the ledger's **tasks** are readable, never writable. The one
thing a reader may add is a user request, which is a row of its own and never a
task field — see [User requests](#user-requests). Three hops, each
GET: the sidecar serves `/session-db/ledger/tasks`, the workload agent relays it
at `/ledger/tasks` (the sidecar binds loopback, so the relay is what the pod
network can reach), and the control plane exposes
`GET /api/v1/sessions/{id}/ledger/tasks`. It takes `limit` and `after_id` only —
the same keyset rules as `ledger_list`, so pass `next_cursor` unchanged and an
empty cursor means done. There is **no SQL on this path**: the view cannot read
anything in the database but the ledger's own task rows, and nothing it serves
changes them. The model is not involved, so a read costs no turn and never
waits on one.

Reading is **passive**, like `Questions` and `ToolApprovals`: it is not an
access and it does not restore a snapshot, so opening a task panel never wakes
a session or moves its idle clock. The consequences are the answers a reader
has to distinguish:

| Answer | Meaning |
| --- | --- |
| 200, empty `tasks` | the slot's ledger really has no tasks |
| 409 | the session keeps a ledger but has no live pod to read it through — the rows are still on the slot's volume, so this is *not* "no tasks" |
| 501 | the session's type keeps no session database at all |
| 400 | a `limit` outside 1–100, or a malformed `after_id`; refused rather than clamped |

The route is scoped like every other session-id route, so another tenant's
session answers exactly like a missing one. Because the ledger belongs to the
**slot**, reading through the pod that holds the slot now is what makes the page
the current chat's ledger rather than a record an earlier session left.

Schema v1 creates only reserved `sp_ledger_*` tables transactionally on first
successful call. It does not change PRAGMA user_version or existing user tables.
A namespace collision or future version is refused without destructive repair.
Any future version needs an explicit transactional migration; never DROP user
tables or silently downgrade. Do not mutate the reserved tables using raw SQL.

## User requests

A person outside the session can leave a request about one task: the words they
typed, stored on the slot until the parent picks it up. It is the chat app's
alternative to speaking into the conversation — nothing is sent as a message and
no turn starts, so a request survives a reply that is still being written, a
restart and a snapshot.

### Storage

Requests live in `sp_ledger_user_requests`, keyed by the caller's own id and
linked to a task by `task_id`. They are **not** part of the task document: a
request arriving while the parent is writing that task with `ledger_put` is a
single INSERT that touches no task row, so neither write loses the other and
`details`, approval and cursors stay exactly as the parent left them. The table
is additive — `sp_ledger_meta` stays at version 1, so an older sidecar image
still opens the database after a rollback and simply ignores the table.

Deleting a task does not delete what a person asked about it: the rows stay
readable by `task_id`. A *new* request about a deleted (retired) task is refused
instead, because nobody would look for it there.

Each row carries id, `task_id`, arrival `seq`, the text, the author, the time,
and then the whole answer to "has this been dealt with": `status`,
`operation_key`/`operation_target`/`dispatch_state` and, once settled, `result`
and `related_seq`. The author is the storing caller's own derivation from the
boundary that authenticated the person; like a task's `owner` it is bookkeeping,
and nothing in the ledger grants a tool, an approval or a merge on the strength
of it. A request's text is a person's input to weigh, not a widening of what the
session may do.

### Write path

Three hops, POST all the way: the sidecar serves
`/session-db/ledger/requests` (loopback only), the workload agent relays it at
`/ledger/requests`, and the control plane exposes
`POST /api/v1/sessions/{id}/ledger/tasks/{taskID}/requests` with
`{id, text, author}`. The task is in the path, so a body cannot aim a request at
another task. Storing is as passive as reading: it is not an access, it restores
no snapshot and it starts no turn.

| Answer | Meaning |
| --- | --- |
| 201 | stored, `pending` |
| 200 | the same id had already stored the same words — the caller's retry, not a second request |
| 404 | no such session, or no such task in this slot's ledger |
| 409 | that id is already used for different words, the task was deleted, or the session has no live pod to store through |
| 501 | the session's type keeps no session database |
| 400 | a malformed id, author or text (1..8192 characters) |

### Lifecycle, and why listing is not taking

`ledger_requests_list` shows requests oldest `seq` first and changes nothing.
Taking one is `ledger_requests_claim` (CAS on `expected_version`, so exactly one
parent wins and the rest get `conflict`); it returns a `claim_token` that
`ledger_requests_dispatch` and `ledger_requests_ack` require. Only an ack
reaches `processed`/`failed`, with the result a person reads and the turn that
carries it — so a request that was looked at and dropped stays visibly open.

`ledger_requests_dispatch` records the intent **before** the outside action:
`operation_key` (the parent's own idempotency key for that action),
`operation_target`, and `dispatch_state` `intended` then `dispatched`. This is
the [external operation reconciliation](#external-operation-reconciliation)
discipline applied to requests, and it is what makes a crash survivable.

A claim that lapses is not a free retry:

- no dispatch recorded → nothing can have happened outside, so the request is
  claimable again as `claimed`;
- a dispatch recorded → the request becomes `reconciling`, and the claim reply
  says `reconcile: true` with the earlier `operation_key`/`operation_target`.
  On a reconciling request a *different* operation key is refused unless
  `observed=not_accepted` states the parent established the earlier dispatch
  never landed. The recorded key is accepted as a retry, but **the key is this
  ledger's own tracking handle, not platform idempotency**: it makes a resend
  safe only where the receiving system itself deduplicates on it. The platform's
  own `POST /sessions/{id}/write` takes no such key, so re-sending a prompt
  there starts another turn — when acceptance is unknown, establish what landed
  before writing again rather than relying on the key.

A lapsed holder's token is refused for both dispatch and ack, so a parent that
comes back late cannot report over the one that took the request meanwhile.

### Discovery, and the parent's order of work

Every `ledger_get`/`ledger_list` task record carries `requests` —
`{pending, claimed, reconciling, last_seq}` — when any request ever arrived. A
parent that only reads its task list still sees that someone asked something;
the counts and `last_seq` move when anything changes.

The order a parent applies, each step its own tool call:

1. On its ledger check, read `requests` on the tasks it owns; a non-zero
   `pending` or `reconciling` is work.
2. `ledger_requests_list` the task, oldest `seq` first.
3. `ledger_requests_claim` the oldest open one. A `reconcile: true` reply is
   handled before anything else: establish what the earlier dispatch did, then
   either ack the outcome found or re-dispatch under the recorded key.
4. Re-read the task (`ledger_get`): a request is new user input, so a `blocked`
   or `review` task is re-evaluated rather than skipped. The request does not
   change the task's status or approval by itself, and an unrelated block is not
   cleared because a request arrived.
5. `ledger_requests_dispatch` with `intended`, then act within the approval
   scope already in force (a tool approval, a human merge gate and a session
   command stay exactly where they were), then `dispatch` again with
   `dispatched`.
6. `ledger_requests_ack` with `processed` or `failed`, the result, and
   `related_seq`.
7. Record the task's own change with `ledger_put` as usual.

## Offered choices

What the app offers a person to say about one task is written **by the session**,
never by the app. `ledger_choices_put` replaces a task's set — up to 6 entries of
{`choice_id`, `label`, `request_text`} — and `expected_version` must be the
task's **current** version, which becomes each entry's `basis_version`.

Reads serve only the entries whose `basis_version` equals the task's current
version. So a task that moves leaves its choices behind automatically: nothing
has to be deleted, a stale sentence is never shown, and a request can never be
sent from one. Writing against a version that no longer holds is a conflict —
read the task again and write the sentences against the version that now holds.
`ledger_choices_clear` empties the set, which the app shows as "none offered"
while keeping free text.

Hiding a stale entry from the read is only half the rule: a screen opened before
the task moved still holds the old sentences, so **the store itself refuses the
pick**. A request carrying `choice_id` is accepted only when that choice is one
the task still offers (its `basis_version` equals the task's current version),
and when the `choice_basis_version` the caller saw agrees with it — which also
refuses a screen whose task re-published the same `choice_id` at a newer
version. The refusal is `stale_choice` (HTTP 409), distinct from a reused id, so
the app can say the screen is out of date, keep the words already typed, and
re-read the task. Free text carries no `choice_id` and is never refused this way.

A choice is a sentence to send, not a permission: it grants no tool approval and
no merge, and the request it fills in is stored like any other
([User requests](#user-requests)) with the `choice_id` beside it, so the session
can tell which of its own sentences came back.

## Task history

`sp_ledger_task_events` is the task's own story, append-only, written **in the
same transaction** as the change it describes: a history row cannot exist for a
change that did not commit, and a replayed mutation adds none. Kinds:
`task_created`, `task_updated` (with what moved), `task_deleted`,
`request_stored` (the person's words), `request_claimed`, `request_dispatched`
(operation key and target — never the claim token), `request_settled` (outcome,
result, related seq), `choices_published`, `choices_cleared`.

It is read by `ledger_events_list({id, after_seq?, limit?})`, and outside the
session through the sidecar's `GET /session-db/ledger/events?task_id=&after_seq=&limit=`,
the agent's `/ledger/events` and
`GET /api/v1/sessions/{id}/ledger/tasks/{taskID}/events`. The cursor is the
row's own seq, so a page boundary neither repeats nor skips a row. Rows are
scoped to the task asked for; no credential, token or transcript reaches them,
and this is a person's task history rather than an agent log.

Rows start accruing when this schema is in place — **earlier work has no
history, and none is invented for it**. A pod still running an earlier image has
no such route; the control plane reports that as unsupported rather than as an
empty history or a missing task.

## Work records (tasks and turns)

The work records are the shape the repository owner agreed on: **a task row and
a turn row, nothing else**. New work is written in them. They live in the same
session DB and are reached through the same `db` MCP, beside the ledger rather
than instead of it.

Why both: the ledger document carries the parent's own bookkeeping — approval
scope, session cursors and seqs, preservation state, monitor rows — and that
does not fit in a task and a turn. Reshaping `sp_ledger_tasks` to the agreed
shape would drop records the parent still acts on, so the ledger tables stay
exactly as they are and a migrated row's parts that have no column here are
preserved verbatim beside it.

### The agreed shape

`sp_work_tasks` — `id`, `session_id` (null means unassigned), `title`,
`description`, `completion_criteria`, `status`, `version`, `created_at`,
`updated_at` (null means never recorded). Status is the ledger's own domain
unchanged — `planned`, `running`, `blocked`, `review`, `completed`, `unknown`:
a migration that renamed a status would move work a person had already seen as
blocked or completed.

`sp_work_turns` — `id`, `task_id`, `session_seq`, `request` (the person's words,
verbatim), `instruction` (what the parent passed to the session doing the work),
`response` (that session's final answer, verbatim), `explanation` (the parent's
own account of the result for the person), `question`, `choices`, `artifacts`,
`requested_at`, `started_at`, `ended_at`. The id is assigned by the database and
increases across the whole database, so it is also the order turns happened in;
a task's turns are indexed by `(task_id, id)`.

**Nothing else is added to the two entities** — a reader of `sp_work_tasks` sees
the agreed columns only. `response` is never a summary the parent wrote; that is
`explanation`. Omitted fields are null, because a turn is written as it happens
rather than all at once. Text bounds exist for the reason the ledger's do: a
slot's storage budget is shared with the session's own archive, so one task
cannot be an attachment.

### API

`task_get`, `task_list`, `task_put`, `task_delete`, `turn_append`, `turn_patch`,
`turn_list`, `turn_dispatch`, and the two migration tools below. Reads change
nothing; `task_get` carries `turns_total` and the latest turn, so a parent sees
at a glance whether a request is waiting, and `turn_list({open: true})` selects
the turns that still need it — a request arrived and the turn has no `ended_at`.
A turn is consumed by writing its response and `ended_at`, not by reading it.

Every mutation takes `request_id`, with the ledger's contract: the same id with
the same arguments returns what was committed then, and the same id with
different arguments is a conflict rather than a second action.

**Exclusion is a compare-and-swap, in three separate lanes.** Comparing a
version without moving it is not a lock, and an earlier revision of this
document was wrong to claim otherwise — measured, two parents holding the same
task version both appended a turn, and both replaced the bookkeeping, with the
later write silently overwriting the earlier one's approval scope. The lanes:

| Write | Holds | Moves |
| --- | --- | --- |
| `task_put`, `task_delete` (task state) | `expected_version` | the task's `version` |
| `turn_append`, `turn_patch`, `turn_dispatch` | `expected_version` **and** `expected_turn_version` | `turn_version` only |
| `work_legacy_put` (parent bookkeeping) | `expected_book_version` | `book_version` only |

Each swap is a single conditional `UPDATE` whose row count is checked, so of two
parents holding the same counter exactly one succeeds and the other is told
which counter to read again. `turn_version` and `book_version` are internal
management counters — not columns of the agreed records — and they are returned
by `task_get`, `turn_list` and `work_legacy_get` so a writer has them before it
writes.

Keeping the lanes apart is the point: a turn write and a cursor advance leave
the task's `version` alone, and the app's offered choices are basis-versioned
against *that*, so routine parent bookkeeping never invalidates a screen nobody
touched. Only a real task change does. `expected_version` on a turn write still
has to match — it says "the task has not changed under me" — but it is the turn
lane's counter that does the excluding.

`task_put` is a full replacement: unnamed fields are not preserved. A deleted
task id is retired permanently, so a later create under it is a conflict rather
than a revival.

A turn's lifecycle is owned, and ownership is a token rather than a timestamp.
Writing `started_at` takes the turn and returns a `claim_token`; taking one that
is already taken is refused, and clearing `started_at` hands it back and
destroys the token. `turn_dispatch` requires that token, the turn to be taken,
and the turn not to have ended. An earlier revision checked only that
`started_at` was set, which is a time and not an identity — measured, a second
caller that merely re-read the counters was allowed to record an outside action
for a turn another parent was working. The token lives in internal management
(`sp_work_turn_claim`), not in the agreed turn, and never appears in a read
view.

Once a key is recorded it keeps naming the same action: the same
`operation_key` may be re-recorded (that is what an idempotency key is for) but
may **not** be pointed at a different `operation_target`, and `dispatched` may
not be rewound to `intended` — unsaying "it left" would let a later reader
believe nothing had. A replayed `request_id` returns the committed answer
**without** moving the counter.

**None of this is external exactly-once, and it is not claimed to be.** What the
slot guarantees is one recorded intent per turn, a single holder who may write
it, no rewind, and a key that cannot change its target. Making the outside
action happen once is the outside system's job, keyed on `operation_key`, which
is why the parent records the key *before* acting. Once a turn has ended, its `request`,
`instruction`, `response` and `explanation` are the record a person was shown
and are not rewritten; reopen it by clearing `ended_at`, or append the next
turn.

### Reading the work from outside the session

The ledger-shaped views have nowhere to put what the agreed model carries, so a
reader that only had them could show that work exists but never the work. The
work records therefore have their own read path, the ledger view's path with the
work records behind it: sidecar `GET /session-db/work/tasks` and
`GET /session-db/work/turns?task_id=` (loopback only, GET only) → the agent's
`/work/tasks` and `/work/turns` → `GET /api/v1/sessions/{id}/work/tasks` and
`GET /api/v1/sessions/{id}/work/tasks/{taskID}/turns`.

A task record carries the agreed columns, `turns_total`, the `latest_turn`, and
`migrated` — which says the task came from the ledger, so a reader knows the
ledger also holds history and parent bookkeeping about it. A turn carries every
agreed field and an `origin`: empty for a turn the session wrote, `user_request`
for one migrated from a ledger request, `app_request` for one a person wrote
through the request route after the cutover. **No claim token, operation key or
dispatch state is on this path** — what the parent handed to another system is
its own bookkeeping and has never been part of what a person reads.

The pages are keyset like the ledger's, a page size out of range is refused
rather than clamped, and a status outside the domain is refused rather than
answered with an empty page. A task with no turns answers an empty page; a task
the slot does not have is **not found**, because an empty page reads as "nothing
has happened yet" and would hide a wrong id. A read while the migration holds
the database is a conflict rather than a half-copied page.

New work's history **is** its turns: a task created after the cutover has no
ledger history and none is invented for it, and a past management event is not
dressed up as a turn. A migrated task keeps its ledger history on the ledger's
own `/ledger/events` route, which is where a reader goes for it.

`turn_dispatch` records what the parent is about to do outside this database
before it does it (`operation_key`, `operation_target`, `dispatch_state`
`intended` → `dispatched`). It is bookkeeping beside the turn, not a turn field,
and it follows [External operation reconciliation](#external-operation-reconciliation):
a different operation key on a turn that already recorded a dispatch is refused
unless `observed=not_accepted` states the earlier one was established never to
have landed.

### Migrating the ledger into them

`work_migrate({request_id, dry_run?})` copies this slot's ledger tasks and the
user requests on them into the work records, once.

**Rehearse first.** `dry_run: true` reports exactly what a run would write — row
counts, the requests it could not map and why, its checks — and changes nothing.

**A real run backs the database up before it writes.** It runs
`PRAGMA integrity_check` and refuses a damaged database rather than copying the
damage, then takes a consistent snapshot with `VACUUM INTO` (one transaction's
view, so the copy is never half a write) to `<db>.pre-work-<UTC timestamp>.bak`
beside the database, and records that file's path, size and SHA-256 in
`sp_work_migration`. The snapshot is **retained**: the migration never deletes
it, and it is what a full restore starts from. Restoring is an offline
operation — no sidecar may have the database open, copy the snapshot over it,
then re-check integrity and row counts.

**Concurrent writes are fenced, not raced.** `sp_work_meta.state` goes `ready` →
`migrating` → `migrated`. While it is `migrating`, ledger and work writes are
refused with `conflict` — a retry for the caller, not a loss, because the
migration is one transaction and takes the ledger with it, so admitting a write
in the middle would copy a row the source no longer has. Reads keep working.

**Interruption is safe and restarting adds nothing.** The fence is committed
before the copy, and the copy is one transaction: an interrupted run rolls back
and leaves the fence up, so the next run resumes from the snapshot already
recorded rather than taking a second one. Every insert is keyed, so a second run
adds no duplicate — it reports what it found already there as `tasks_present`
and `turns_present`. A fence that is up with no snapshot recorded is refused
outright: a copy with no recorded starting point has nothing to roll back to.

**Nothing is deleted or flattened.** The ledger tables stay exactly as they are,
and every part of a row that has no column in the agreed shape — stage, owner,
the session relations with their cursors and seqs, the details JSON with
approval and preservation state, a request's claim and dispatch — is preserved
verbatim in `sp_work_task_legacy` / `sp_work_turn_legacy`. Reading the work
records never requires them; the parent still acts on them.

**Only what the ledger really recorded is mapped.** A monitor row is the
parent's own bookkeeping rather than a person's work, so it is preserved and
does not become a task — presenting one as a task would invent work the person
never asked for. The ledger never kept the instruction that went to a session or
that session's final answer verbatim, so `instruction` and `response` stay null.
A request's `result` is the sentence the parent wrote for the person, so it
becomes the turn's `explanation` — never its `response`, which would dress a
parent's summary up as the session's own words. A time or a seq the ledger does
not have stays null rather than being guessed. Past management events are not
turns: only a stored user request — a person's own words, with the time they
arrived — becomes one. Status, approval and completion do not move because the
data did.

**What it did is readable afterwards.** The run verifies its own counts before
committing, and `work_migration_report` gives the schema state, every recorded
step with its row counts and the snapshot's checksum, and the current counts so
a reader can re-check them. A second `work_migrate` on a migrated database
returns that report with `already_migrated`.

### After the cutover, and rolling back

A migrated slot **keeps its ledger readable** — the monitor rows and the task
history the parent works from live only there — and a ledger write is refused
only where the two records could actually diverge:

- A task the work records own is refused, and the refusal names what to use
  instead (`task_put`, `turn_*`, and `work_legacy_put` for its bookkeeping).
- Choices and user requests are refused outright: a choice lives on a turn now
  and a request becomes one.
- Creating a **new** ledger task is refused, because the app reads the work
  records and a task written to the ledger now would be work nobody can see.
- A preserved row the work records do **not** own — a monitor row, which is the
  parent's own bookkeeping and was never a person's work — stays writable
  through the tools it always had. Nothing in the work records reads it, so
  there is nothing to diverge from, and refusing it would leave the parent
  unable to record that its own loops ran.

The parent's bookkeeping beside a migrated task — approval scope, the session
relations with their cursors and seqs, preservation and cancellation state — is
read with `work_legacy_get` and replaced with `work_legacy_put`, under the work
task's version. Writing it does **not** move that version: it is a record beside
the task, not a new state of it, so advancing a cursor does not silently
invalidate the choices a turn is offering. **Preserved and unreachable would be
the same as lost**, which is why this path exists rather than only the
preservation. None of it is on the read path a person sees: no claim token,
operation key or dispatch state leaves through the work views.

The app and its API are unchanged across the cutover. The read-only HTTP view
serves work tasks projected into the same page shape, with the preserved
bookkeeping put back where a reader of that shape expects it, so a caller cannot
tell from the answer whether this slot has migrated — which is what lets the
image and the app roll out separately. A stored user request becomes the next
turn rather than a row beside a task that no longer exists here. A projected
choice id names the turn it came from, so a pick from a turn that is no longer
the latest is refused rather than silently applied. The one deploy order that
does not work is an app expecting work views against an older image, which
answers 404 — the app reads that as "no route", not as "no work".

`sp_ledger_meta` stays at version 1 and these tables are additive, for the
reason the request and choice tables kept it there: an image rolled back to one
that never heard of them still opens the database and still serves the ledger,
whose rows the migration left intact. `sp_work_meta` carries this namespace's
own version, and a database written by a later version is refused rather than
read with the columns of this one.

**That is necessary and not sufficient.** The ledger such an image serves is the
one the migration left behind, so everything written *after* the cutover — a
task created in the work records, a turn, a person's request that became a turn
— sits in tables it does not read. Rolling back on that basis alone would show
a person work that silently stopped at the cutover, and leave the parent acting
on a ledger that has forgotten days of exchanges. Invisible is the same as lost
to whoever is reading the screen.

So a rollback is a step, not an accident:

1. `work_rollback_export({request_id, dry_run?})`. The rehearsal reports exactly
   what it would write and changes nothing. A real run snapshots first
   (`<db>.pre-rollback-<UTC>.bak`, checksummed and retained — a name already in
   use takes the next free one rather than overwriting a snapshot something may
   still need), then in one transaction writes every work task back to its
   `sp_ledger_tasks` row and every turn holding a person's words back to
   `sp_ledger_user_requests`. The agreed fields the ledger has no column for
   travel in the document's `details.work_records`, so a description is not
   dropped at the moment the ledger takes over. A turn with **no** request is
   something the ledger shape never modelled; it is counted as
   `turns_unexportable` and named, not dropped quietly, and it stays in the work
   records.
2. The state becomes `rolled_back`. From then on the direction is explicit
   rather than guessed: **work writes are refused** with a message saying the
   ledger is authoritative, and **ledger writes are allowed again**, so the older
   image can keep working and there is never a second writer.
3. Roll the image back. A person's new request lands as a ledger row, and the
   parent uses the ledger tools, exactly as before the cutover.
4. Coming forward is `work_migrate` again. Every insert is keyed, so nothing
   already mapped is duplicated — including the rows the export itself wrote,
   which are recorded as `exported_turn` so they come back as the turns they
   are rather than as new ones. Because the ledger was authoritative in the
   meantime, this run **reconciles** rather than resumes: a task that is already
   there is refreshed from the ledger (`tasks_reconciled`), so an edit the older
   image made is imported instead of skipped.

The two records are therefore never both writable, and which one is
authoritative is a value in `sp_work_meta.state` rather than an assumption.
This round trip — migrate, work after the cutover, export, ledger writes by the
older image, migrate forward — is exercised on an isolated fixture database and
asserts that nothing is lost and nothing is duplicated. It is not a claim about
a deployed restore drill on slot storage, which stays in the post-merge
verification below.

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

Local tests exercise the default surface (ledger only, raw calls refused with
the opt-in named) and the debug surface, the read-only HTTP view (same pages as
`ledger_list`, GET only, no SQL reachable, `limit` refused rather than clamped,
non-loopback refused, proxy passthrough intact) and its control-plane route
(pages and cursor, ownership scoping, 501/409 distinguished from an empty
ledger, and a read leaving a snapshot untouched), preservation of user
tables/user_version, schema refusal,
transaction rollback on receipt failure, single-winner CAS, replay and ID reuse,
validation, filtering/keyset pages, raw transaction cleanup and close/reopen.
Pod-spec tests cover the slot-level mount. These do not claim a deployed NFS restart/snapshot/restore test.

For the work records they cover the agreed columns being exactly those columns,
task and turn CRUD with version CAS and retirement, global turn ordering, the
open-turn queue, single-winner concurrent writes, replay, validation and schema
refusal; and for the migration: the dry run changing nothing, mapping only what
the ledger recorded, task times coming from recorded history, fresh choices
moving onto the latest turn, incomplete legacy rows, a damaged database refused,
idempotence and resumption, the fence refusing writes, a fence with no snapshot
refused, user tables left alone, and the ledger left intact for a rollback.
The cutover is covered from the app's side: ledger writes refused while reads
keep working, the projected page and stored-request-as-turn path, and a pick
from an older turn refused. The snapshot path is exercised against a local
SQLite file; **this does not claim a deployed restore drill on slot storage**,
which belongs to the post-merge verification below.

After authorized merge/deployment, verify tool discovery in the parent,
restart and snapshot/restore with the same slot, and that a deleted and
recreated chat session on the same slot reads the earlier session's rows.
Refresh parent skills, then list/read changed skills and references. A skill
refresh alone does not deploy runtime tools; before availability use the skill's
conversation-record fallback. This implementation session does not create test
cluster sessions, merge, deploy, or refresh the parent.
