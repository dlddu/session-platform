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
	"database/sql"
	"encoding/json"
	"errors"
	"net/http"
)

const (
	// sessionDBWorkWriterPath reads and sets who may write this slot's work
	// records. GET is the fact; POST is the handover or its rollback.
	sessionDBWorkWriterPath = "/session-db/work/writer"
	// sessionDBWorkCallPath is one work tool call made by the bound monitor.
	sessionDBWorkCallPath = "/session-db/work/call"
)

// serveWorkWriter answers or moves the seal.
//
// The POST is the ordering guarantee the handover needs: when it answers 200 the
// previous writer is already refused, because this call and every mutation of
// these records contend for the same database. A caller may open the next writer
// as soon as it has this answer, and has no reason to wait a fixed time for
// anything to propagate.
func (s *sessionDB) serveWorkWriter(w http.ResponseWriter, r *http.Request) {
	if !fromLoopback(r) {
		http.Error(w, "the work writer view is only for this pod", http.StatusForbidden)
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
	if !fromLoopback(r) {
		http.Error(w, "the work call view is only for this pod", http.StatusForbidden)
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
