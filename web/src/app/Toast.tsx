// Toast infrastructure ported from docs/mockups (toast-wrap / toast). Provides a
// context + useToast() hook so any screen can surface a transient notice. Only
// the component + plumbing live here; triggers are wired by user actions in the
// screens (the mockup's demo-only simulated triggers are intentionally omitted).
import {
  createContext,
  useCallback,
  useContext,
  useRef,
  useState,
  type ReactNode,
} from "react";

export type ToastKind = "ok" | "frozen" | "warm";

interface ToastItem {
  id: number;
  msg: string;
  kind: ToastKind;
  in: boolean;
}

interface ToastApi {
  toast: (msg: string, kind?: ToastKind) => void;
}

const ToastContext = createContext<ToastApi | null>(null);

// useToast surfaces the toast() dispatcher; throws if used outside the provider
// so a missing <ToastProvider> fails loudly instead of silently no-op'ing.
export function useToast(): ToastApi {
  const ctx = useContext(ToastContext);
  if (!ctx) throw new Error("useToast must be used within <ToastProvider>");
  return ctx;
}

function ToastIcon({ kind }: { kind: ToastKind }) {
  if (kind === "frozen") {
    return (
      <svg width="16" height="16" viewBox="0 0 24 24" fill="none" stroke="currentColor" strokeWidth="1.7" strokeLinecap="round" strokeLinejoin="round">
        <path d="M12 2v20M4.2 7l15.6 10M19.8 7 4.2 17" />
      </svg>
    );
  }
  if (kind === "warm") {
    return (
      <svg width="16" height="16" viewBox="0 0 24 24" fill="none" stroke="currentColor" strokeWidth="2" strokeLinecap="round" strokeLinejoin="round">
        <path d="M12 2 21 7v10l-9 5-9-5V7l9-5Z" />
        <circle cx="12" cy="12" r="2" />
      </svg>
    );
  }
  return (
    <svg width="16" height="16" viewBox="0 0 24 24" fill="none" stroke="currentColor" strokeWidth="2.2" strokeLinecap="round" strokeLinejoin="round">
      <path d="M20 6 9 17l-5-5" />
    </svg>
  );
}

export function ToastProvider({ children }: { children: ReactNode }) {
  const [items, setItems] = useState<ToastItem[]>([]);
  const seq = useRef(0);

  const toast = useCallback((msg: string, kind: ToastKind = "ok") => {
    const id = ++seq.current;
    setItems((prev) => [...prev, { id, msg, kind, in: false }]);
    // Next frame: flip `in` to trigger the enter transition (matches mockup's
    // requestAnimationFrame nudge).
    requestAnimationFrame(() =>
      setItems((prev) => prev.map((t) => (t.id === id ? { ...t, in: true } : t)))
    );
    // Auto-dismiss: start the exit transition, then drop after it finishes.
    setTimeout(() => {
      setItems((prev) => prev.map((t) => (t.id === id ? { ...t, in: false } : t)));
      setTimeout(() => setItems((prev) => prev.filter((t) => t.id !== id)), 350);
    }, 3200);
  }, []);

  return (
    <ToastContext.Provider value={{ toast }}>
      {children}
      <div className="toast-wrap" id="toasts">
        {items.map((t) => (
          <div key={t.id} className={`toast ${t.kind}${t.in ? " in" : ""}`} role="status">
            <span className="ic">
              <ToastIcon kind={t.kind} />
            </span>
            <span>{t.msg}</span>
          </div>
        ))}
      </div>
    </ToastContext.Provider>
  );
}
