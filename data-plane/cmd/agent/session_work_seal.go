// Who may write this slot's work records, recorded in the records themselves.
//
// The separated monitor takes over a session's work. Until now the only thing
// that said so was an environment variable the control plane put on the pod at
// creation, which is settled for a session that starts paired and useless for
// one that is already running: the process keeps the role it was born with, so
// a parent whose monitor was bound a minute ago goes on writing.
//
// Waiting out a propagation delay would not fix it either — "long enough" is a
// guess about another machine, and a guess cannot be the evidence that the old
// writer stopped. So the fact is kept where the writes happen. One row says who
// the writer is; every mutation checks it inside its own transaction, which
// SQLite serialises against every other write to this database. A handover is
// one transaction:
//
//   - before it commits, the session's own calls are admitted and the monitor's
//     are not;
//   - after it commits, the session's own calls are refused and the monitor's
//     are admitted;
//   - there is no third state, and no write can straddle the two, because the
//     seal and the write take the same lock.
//
// That is the whole mechanism for "stop the old writer before starting the new
// one". A write in flight either committed before the seal or meets it and is
// refused; it cannot land after. A restart changes nothing: the row is on disk,
// so the refusal survives the process that was told its role by env.
//
// The epoch makes the handover idempotent and ordered. Sealing with the epoch
// already recorded is the same seal, so a retried handover is not a second one;
// sealing with a stale epoch is refused, so two controllers racing cannot swap
// the writer back and forth unnoticed.
//
// Scope, stated plainly: this is a boundary against a stale writer, not against
// a hostile one. The workload container shares the pod's loopback with the
// sidecar, so a session that set out to forge a handover could call the same
// route the control plane calls. Closing that needs an identity the workload
// cannot mint, which this pod layout does not have (the pair token is projected
// into the workload container only, deliberately — see
// control-plane/internal/adapter/k8s/monitor_pair.go). What this does close is
// the real failure: an honest parent that has not yet heard it was paired.
package main

import (
	"context"
	"database/sql"
	"errors"
	"strconv"
	"strings"
	"time"
)

const (
	// workWriterSession is the ordinary state: this session writes its own work
	// records, which is every session that has no monitor.
	workWriterSession = "session"
	// workWriterMonitor means a bound monitor owns them. The session's own calls
	// are refused and the monitor's are admitted.
	workWriterMonitor = "monitor"
)

// workWriter is the one row, as a reader sees it.
type workWriter struct {
	Writer    string `json:"writer"`
	Epoch     int64  `json:"epoch"`
	Holder    string `json:"holder,omitempty"`
	ChangedAt string `json:"changed_at,omitempty"`
}

// Sealed reports whether the session's own writes are closed.
func (w workWriter) Sealed() bool { return w.Writer == workWriterMonitor }

// readWorkWriter answers the row, creating the default on first read. A slot
// that has never been handed over reads as the session's own, which is what
// every existing deployment is.
func readWorkWriter(ctx context.Context, tx *sql.Tx) (workWriter, error) {
	var got workWriter
	err := tx.QueryRowContext(ctx,
		"SELECT writer,epoch,holder,changed_at FROM sp_work_writer WHERE id=1").
		Scan(&got.Writer, &got.Epoch, &got.Holder, &got.ChangedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return workWriter{Writer: workWriterSession, Epoch: 0}, nil
	}
	if err != nil {
		return workWriter{}, err
	}
	return got, nil
}

// setWorkWriter writes the row at the next epoch. The caller has already
// checked the epoch it was acting on, inside this same transaction.
func setWorkWriter(ctx context.Context, tx *sql.Tx, writer, holder string, epoch int64, now time.Time) error {
	_, err := tx.ExecContext(ctx,
		`INSERT INTO sp_work_writer (id,writer,epoch,holder,changed_at) VALUES (1,?,?,?,?)
		 ON CONFLICT(id) DO UPDATE SET
		   writer=excluded.writer,epoch=excluded.epoch,holder=excluded.holder,changed_at=excluded.changed_at`,
		writer, epoch, holder, workNow(now))
	return err
}

// workSealInput is a handover or its rollback, as the control plane states it.
type workSealInput struct {
	// Writer is who may write after this call: "monitor" hands the records to
	// the bound monitor, "session" gives them back.
	Writer string `json:"writer"`
	// Holder is the monitor session the records are handed to. It is recorded so
	// a reader of this slot can say which pair sealed it, and so a rollback can
	// be checked against the monitor that actually holds it.
	Holder string `json:"holder"`
	// ExpectedEpoch is the epoch the caller read. Omitted on a first handover of
	// a slot that has never been sealed.
	ExpectedEpoch *int64 `json:"expected_epoch,omitempty"`
}

// sealWorkWriter performs one handover, or one rollback, atomically.
//
// Ordering is the point. Returning success means the previous writer is already
// refused — not that it will be shortly — because this function and every
// mutation of these records take the same database. So a caller may open the
// next writer the moment this returns, and must not before.
func sealWorkWriter(ctx context.Context, tx *sql.Tx, in workSealInput, now time.Time) (any, error) {
	writer := strings.TrimSpace(in.Writer)
	holder := strings.TrimSpace(in.Holder)
	switch writer {
	case workWriterMonitor:
		if holder == "" {
			return nil, workFail("invalid_argument",
				"writer=monitor must name the monitor session the records are handed to")
		}
	case workWriterSession:
		holder = ""
	default:
		return nil, workFail("invalid_argument", `writer must be "monitor" or "session"`)
	}
	current, err := readWorkWriter(ctx, tx)
	if err != nil {
		return nil, err
	}
	if in.ExpectedEpoch != nil && *in.ExpectedEpoch != current.Epoch {
		return nil, workFail("conflict",
			"expected_epoch does not match this slot's writer epoch; read the writer again before sealing")
	}
	// The same seal again is the same seal. A retry after a lost reply must not
	// move the epoch, or the caller that retried would report a handover that
	// did not happen and a reader would see the writer change twice.
	if current.Writer == writer && current.Holder == holder {
		return map[string]any{"writer": current, "changed": false}, nil
	}
	// A rollback has to name the monitor that holds the records. Handing them
	// back from a pair that does not hold them would unseal someone else's
	// handover.
	if writer == workWriterSession && current.Sealed() &&
		strings.TrimSpace(in.Holder) != "" && in.Holder != current.Holder {
		return nil, workFail("conflict",
			"these records are held by "+current.Holder+", not "+in.Holder)
	}
	next := current.Epoch + 1
	if err := setWorkWriter(ctx, tx, writer, holder, next, now); err != nil {
		return nil, err
	}
	sealed, err := readWorkWriter(ctx, tx)
	if err != nil {
		return nil, err
	}
	return map[string]any{"writer": sealed, "changed": true}, nil
}

// refuseSealedWorkWrite is the gate, inside the caller's own transaction.
//
// It replaces nothing: the role-env refusal still runs first and still answers
// before a transaction is opened, which keeps the common paired case cheap and
// its message unchanged. This is the case that one cannot see — a session whose
// pod was started before the pair existed.
func refuseSealedWorkWrite(ctx context.Context, tx *sql.Tx, name, caller string) error {
	if !isWorkMutation(name) {
		return nil
	}
	current, err := readWorkWriter(ctx, tx)
	if err != nil {
		return err
	}
	switch {
	case current.Sealed() && caller != workCallerMonitor:
		return workFail("permission_denied",
			"this session's work records were handed to its monitor "+current.Holder+
				" at "+current.ChangedAt+" (writer epoch "+strconv.FormatInt(current.Epoch, 10)+"); "+name+
				" is the monitor's to call. Send an instruction with the monitor_pair request_send "+
				"tool instead, and read with task_get, task_list or turn_list")
	case !current.Sealed() && caller == workCallerMonitor:
		return workFail("permission_denied",
			"these work records are the session's own; a monitor may write them only after the "+
				"control plane has handed them over")
	}
	return nil
}

// workCallerSelf and workCallerMonitor are the two callers of the work surface:
// the session's own model through the slot MCP, and a bound monitor through the
// control plane. The caller is decided by the route the call arrived on, never
// by anything in the call.
const (
	workCallerSelf    = "self"
	workCallerMonitor = "monitor"
)
