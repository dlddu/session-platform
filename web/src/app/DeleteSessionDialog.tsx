// mockup: none — 삭제 확인 대화상자는 SPA에만 있고 대응 목업이 없다
import { useEffect, useId, useRef, useState } from "react";
import { api } from "../api/client";
import type { Session } from "../api/types";

interface DeleteSessionDialogProps {
  session: Session;
  onCancel: () => void;
  onDeleted: (session: Session) => void;
}

export function DeleteSessionDialog({
  session,
  onCancel,
  onDeleted,
}: DeleteSessionDialogProps) {
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState<string | null>(null);
  const modalRef = useRef<HTMLDivElement>(null);
  const cancelRef = useRef<HTMLButtonElement>(null);
  const confirmRef = useRef<HTMLButtonElement>(null);
  const returnFocusRef = useRef<HTMLElement | null>(null);
  const busyRef = useRef(busy);
  const onCancelRef = useRef(onCancel);
  const titleId = useId();
  const descriptionId = useId();

  busyRef.current = busy;
  onCancelRef.current = onCancel;

  useEffect(() => {
    returnFocusRef.current =
      document.activeElement instanceof HTMLElement
        ? document.activeElement
        : null;
    cancelRef.current?.focus();

    function handleKeyDown(event: KeyboardEvent) {
      if (event.key === "Escape") {
        if (!busyRef.current) {
          event.preventDefault();
          onCancelRef.current();
        }
        return;
      }
      if (event.key !== "Tab") return;

      const focusable = Array.from(
        modalRef.current?.querySelectorAll<HTMLElement>(
          "button:not(:disabled)",
        ) ?? [],
      );
      if (focusable.length === 0) {
        event.preventDefault();
        modalRef.current?.focus();
        return;
      }

      const first = focusable[0];
      const last = focusable[focusable.length - 1];
      const active = document.activeElement;
      if (!modalRef.current?.contains(active)) {
        event.preventDefault();
        (event.shiftKey ? last : first).focus();
        return;
      }
      if (event.shiftKey && active === first) {
        event.preventDefault();
        last.focus();
      } else if (!event.shiftKey && active === last) {
        event.preventDefault();
        first.focus();
      }
    }

    document.addEventListener("keydown", handleKeyDown);
    return () => {
      document.removeEventListener("keydown", handleKeyDown);
      returnFocusRef.current?.focus();
    };
  }, []);

  async function confirmDelete() {
    setBusy(true);
    setError(null);
    try {
      await api.deleteSession(session.id);
      onDeleted(session);
    } catch (requestError) {
      setError(String(requestError));
      setBusy(false);
    }
  }

  function cancel() {
    if (!busy) onCancel();
  }

  return (
    <div
      className="scrim"
      onClick={(event) => {
        if (event.target === event.currentTarget) cancel();
      }}
    >
      <div
        className="modal delete-modal"
        ref={modalRef}
        tabIndex={-1}
        role="alertdialog"
        aria-modal="true"
        aria-labelledby={titleId}
        aria-describedby={descriptionId}
        data-testid="delete-session-dialog"
      >
        <div className="delete-mark" aria-hidden="true">
          <svg
            width="22"
            height="22"
            viewBox="0 0 24 24"
            fill="none"
            stroke="currentColor"
            strokeWidth="2"
            strokeLinecap="round"
            strokeLinejoin="round"
          >
            <path d="M3 6h18M8 6V4h8v2M6 6l1 15h10l1-15M10 10v7M14 10v7" />
          </svg>
        </div>
        <h3 id={titleId}>Delete session?</h3>
        <p className="desc" id={descriptionId}>
          This removes the session and reclaims its live pod, if any. It will no
          longer be accessible or restorable.
        </p>
        <div className="delete-target">
          <strong>{session.name}</strong>
          <span>session/{session.id}</span>
        </div>
        {error ? (
          <div
            className="delete-error"
            role="alert"
            data-testid="delete-session-error"
          >
            Delete failed: {error}
          </div>
        ) : null}
        <div className="modal-actions">
          <button
            type="button"
            className="btn btn-ghost"
            onClick={cancel}
            disabled={busy}
            ref={cancelRef}
            data-testid="delete-session-cancel"
          >
            Cancel
          </button>
          <button
            type="button"
            className="btn btn-danger"
            onClick={() => void confirmDelete()}
            disabled={busy}
            ref={confirmRef}
            data-testid="delete-session-confirm"
          >
            {busy ? "Deleting…" : "Delete session"}
          </button>
        </div>
      </div>
    </div>
  );
}
