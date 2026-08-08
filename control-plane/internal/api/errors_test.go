package api

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/dlddu/session-platform/control-plane/internal/session"
)

func TestWriteErrPreservesPublicStatus(t *testing.T) {
	for _, tc := range []struct {
		name string
		err  error
		want int
	}{
		{name: "queue full", err: session.ErrWorkloadQueueFull, want: http.StatusTooManyRequests},
		{name: "prompt too large", err: session.ErrWorkloadPromptTooLarge, want: http.StatusRequestEntityTooLarge},
		{name: "request body too large", err: session.ErrRequestBodyTooLarge, want: http.StatusRequestEntityTooLarge},
		{name: "output full", err: session.ErrWorkloadOutputFull, want: http.StatusInsufficientStorage},
	} {
		t.Run(tc.name, func(t *testing.T) {
			recorder := httptest.NewRecorder()
			writeErr(recorder, errors.Join(errors.New("adapter context"), tc.err))
			if recorder.Code != tc.want {
				t.Fatalf("status = %d, want %d", recorder.Code, tc.want)
			}
		})
	}
}

func TestDecodeRequestBodyIsStrictButAllowsOptionalEmptyBody(t *testing.T) {
	for _, tc := range []struct {
		name string
		body string
		want error
	}{
		{name: "empty optional"},
		{name: "explicit null", body: `null`, want: session.ErrInvalidInput},
		{name: "null field", body: `{"payload":null}`, want: session.ErrInvalidInput},
		{name: "known field", body: `{"payload":"ok"}`},
		{name: "unknown field", body: `{"payload":"ok","model":"other"}`, want: session.ErrInvalidInput},
		{name: "malformed", body: `{"payload":`, want: session.ErrInvalidInput},
		{name: "trailing value", body: `{"payload":"ok"}{}`, want: session.ErrInvalidInput},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var req writeReq
			err := decodeRequestBody(strings.NewReader(tc.body), &req, false)
			if !errors.Is(err, tc.want) {
				t.Fatalf("error = %v, want %v", err, tc.want)
			}
		})
	}

	var read readReq
	if err := decodeRequestBody(strings.NewReader(`{"offset":null}`), &read, false); !errors.Is(err, session.ErrInvalidInput) {
		t.Fatalf("null offset error = %v, want %v", err, session.ErrInvalidInput)
	}
}

func TestDecodeRequestBodyEnforcesWireLimit(t *testing.T) {
	var req writeReq
	err := decodeRequestBody(strings.NewReader(strings.Repeat(" ", maxRequestBodyBytes+1)), &req, false)
	if !errors.Is(err, session.ErrRequestBodyTooLarge) {
		t.Fatalf("error = %v, want %v", err, session.ErrRequestBodyTooLarge)
	}
}

func TestCreateSessionPreservesWireLimitStatus(t *testing.T) {
	req := httptest.NewRequest(http.MethodPost, "/api/v1/sessions", strings.NewReader(strings.Repeat(" ", maxRequestBodyBytes+1)))
	recorder := httptest.NewRecorder()

	(&API{}).createSession(recorder, req)

	if recorder.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("status = %d, want %d", recorder.Code, http.StatusRequestEntityTooLarge)
	}
}
