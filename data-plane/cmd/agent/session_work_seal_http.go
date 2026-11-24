// The two routes the control plane uses to hand a session's work records to its
// monitor and to take them back, and the route the monitor writes them through.
//
// Both sit beside the existing work views on the sidecar (session_work_http.go)
// and are loopback-only for the same reason: the database is the slot's, the
// sidecar is the only container that mounts it, and the agent on the pod network
// is what relays a caller from outside.
//
// Why the monitor writes through a route of its own rather than the slot MCP:
// the records have exactly one writer at a time, and which caller is which has
// to be decided by something a caller cannot say about itself. The slot MCP is
// the session's own model; this route is the control plane acting for the bound
// monitor. The seal row says which of the two may write (session_work_seal.go),
// so the pair never has two writers, and the session's own model keeps reading
// its own records the whole time — before the handover and after it, out of the
// same tables, which is why no history has to move anywhere.
package main

import (
	"context"
	"crypto/subtle"
	"database/sql"
	"encoding/json"
	"errors"
	"net/http"
	"os"
	"strings"
)

const (
	// sessionDBWorkWriterPath reads and sets who may write this slot's work
	// records. GET is the fact; POST is the handover or its rollback.
	sessionDBWorkWriterPath = "/session-db/work/writer"
	// sessionDBWorkCallPath is one work tool call made by the bound monitor.
	sessionDBWorkCallPath = "/session-db/work/call"
	// monitorPairWriterTokenEnv is the credential these two routes require. The
	// orchestrator puts it on this container and not on the workload container
	// (control-plane/internal/adapter/k8s: MonitorPairWriterTokenEnvVar), so a
	// caller that merely shares the pod cannot move the writer or write as the
	// monitor. Unset means this slot cannot be handed over at all.
	monitorPairWriterTokenEnv = "MONITOR_PAIR_WRITER_TOKEN"
)

// authorizeWriterCall is the gate in front of both seal routes.
//
// Loopback stays required — the database is this pod's — but it is not what
// authorises the call: the workload container is on the same loopback. The
// credential is, and it is compared whole and in constant time.
//
// A sidecar with no credential configured refuses rather than serving these
// routes open. That is deliberate: the control plane reads the refusal as "this
// slot cannot be handed over", and a pair is not bound over a slot it could not
// seal — so an unprepared deployment declines the feature instead of running it
// without the boundary.
func authorizeWriterCall(w http.ResponseWriter, r *http.Request) bool {
	if !fromLoopback(r) {
		http.Error(w, "the work writer view is only for this pod", http.StatusForbidden)
		return false
	}
	want := strings.TrimSpace(os.Getenv(monitorPairWriterTokenEnv))
	if want == "" {
		writeWorkViewError(w, http.StatusNotImplemented, "unsupported",
			"this slot has no writer credential, so its work records cannot be handed over")
		return false
	}
	got := strings.TrimSpace(strings.TrimPrefix(
		strings.TrimSpace(r.Header.Get("Authorization")), "Bearer "))
	if subtle.ConstantTimeCompare([]byte(got), []byte(want)) != 1 {
		// Being in the pod is not enough, and saying which part failed would
		// help a caller in the pod guess its way in.
		writeWorkViewError(w, http.StatusForbidden, "permission_denied",
			"only the control plane may move this slot's writer or write as its monitor")
		return false
	}
	return true
}

// serveWorkWriter answers or moves the seal.
//
// The POST is the ordering guarantee the handover needs: when it answers 200 the
// previous writer is already refused, because this call and every mutation of
// these records contend for the same database. A caller may open the next writer
// as soon as it has this answer, and has no reason to wait a fixed time for
// anything to propagate.
func (s *sessionDB) serveWorkWriter(w http.ResponseWriter, r *http.Request) {
	if !authorizeWriterCall(w, r) {
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), sessionDBCallTimeout)
	defer cancel()
	switch r.Method {
	case http.MethodGet:
		s.answerWorkWriter(w, ctx, nil)
	case http.MethodPost:
		var body workSealInput
		decoder := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<16))
		decoder.DisallowUnknownFields()
		if err := decoder.Decode(&body); err != nil {
			writeWorkViewError(w, http.StatusBadRequest, "invalid_argument",
				"body must be {writer, holder, expected_epoch?}")
			return
		}
		s.answerWorkWriter(w, ctx, &body)
	default:
		w.Header().Set("Allow", "GET, POST")
		http.Error(w, "the work writer view accepts GET and POST", http.StatusMethodNotAllowed)
	}
}

// answerWorkWriter runs the read, or the seal, in one transaction with the
// schema gate in front of it — the same gate every work call passes.
func (s *sessionDB) answerWorkWriter(w http.ResponseWriter, ctx context.Context, seal *workSealInput) {
	db, err := s.handle(ctx)
	if err != nil {
		writeWorkSealError(w, s, err)
		return
	}
	var result any
	err = s.workTx(ctx, db, func(tx *sql.Tx) error {
		if seal == nil {
			current, readErr := readWorkWriter(ctx, tx)
			if readErr != nil {
				return readErr
			}
			result = map[string]any{"writer": current, "changed": false}
			return nil
		}
		sealed, sealErr := sealWorkWriter(ctx, tx, *seal, workClock())
		if sealErr != nil {
			return sealErr
		}
		result = sealed
		return nil
	})
	if err != nil {
		writeWorkSealError(w, s, err)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(result); err != nil {
		s.logger.Info("work writer reply could not be written", "err", err)
	}
}

// workCallBody is one work tool call: the tool's name and the arguments an MCP
// caller would pass, validated by exactly the same code.
type workCallBody struct {
	Name string          `json:"name"`
	Args json.RawMessage `json:"args"`
}

// serveWorkCall runs one work tool as the bound monitor.
//
// It adds no tool and relaxes no rule: the call goes through callWork, so the
// argument schema, the receipts, both compare-and-swap lanes and the seal check
// are the ones the session's own model meets. The only difference is the caller
// it is attributed to, which decides which side of the seal may write.
func (s *sessionDB) serveWorkCall(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", http.MethodPost)
		http.Error(w, "the work call view accepts POST", http.StatusMethodNotAllowed)
		return
	}
	if !authorizeWriterCall(w, r) {
		return
	}
	var body workCallBody
	decoder := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxWorkViewBytes))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&body); err != nil {
		writeWorkViewError(w, http.StatusBadRequest, "invalid_argument", "body must be {name, args}")
		return
	}
	if !isWorkTool(body.Name) {
		writeWorkViewError(w, http.StatusBadRequest, "invalid_argument", "name must be a work tool")
		return
	}
	if len(body.Args) == 0 {
		body.Args = json.RawMessage("{}")
	}
	ctx, cancel := context.WithTimeout(r.Context(), sessionDBCallTimeout)
	defer cancel()
	out, rpcErr := s.callWork(ctx, workCallerMonitor, body.Name, body.Args)
	if rpcErr != nil {
		writeWorkViewError(w, http.StatusBadRequest, "invalid_argument", "the call could not be made")
		return
	}
	// callWork answers an MCP tool result, which already distinguishes a refusal
	// from an answer. Pass it through: the monitor's caller reads the same codes
	// the session's own model reads, and a refusal stays a refusal.
	result, _ := out.(map[string]any)
	status := http.StatusOK
	if result["isError"] == true {
		status = http.StatusConflict
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	if err := json.NewEncoder(w).Encode(result); err != nil {
		s.logger.Info("work call reply could not be written", "err", err)
	}
}

func writeWorkSealError(w http.ResponseWriter, s *sessionDB, err error) {
	var known workError
	if errors.As(err, &known) {
		writeWorkViewError(w, workViewStatus(known.Code), known.Code, known.ErrorText)
		return
	}
	s.logger.Info("work writer call failed", "err", err)
	writeWorkViewError(w, http.StatusInternalServerError, "storage_error", "the writer could not be read or set")
}
