// mockup: docs/mockups/index.html
import { createContext, useContext, useEffect, useState } from "react";
import { NavLink, Outlet, useLocation } from "react-router-dom";
import { api } from "../api/client";
import { LockIcon } from "./icons";
import { ToastProvider } from "./Toast";
import "./shell.css";

export const REPO_ACCESS_PATH = "/access";

type SetRepoAccessBadge = (pending: number | null) => void;

const RepoAccessBadgeContext = createContext<SetRepoAccessBadge | null>(null);

export function useRepoAccessBadge(): SetRepoAccessBadge {
  const set = useContext(RepoAccessBadgeContext);
  if (!set) throw new Error("useRepoAccessBadge must be used within <AppShell>");
  return set;
}

export function AppShell() {
  const { pathname } = useLocation();
  const [pending, setPending] = useState<number | null>(null);

  useEffect(() => {
    if (pathname === REPO_ACCESS_PATH) return;
    let cancelled = false;
    api
      .getRepoAccess()
      .then((state) => {
        if (!cancelled) setPending(state.requests.length);
      })
      .catch(() => {
        if (!cancelled) setPending(null);
      });
    return () => {
      cancelled = true;
    };
  }, [pathname]);

  return (
    <div className="app">
      <div className="viewport">
        <ToastProvider>
          <RepoAccessBadgeContext.Provider value={setPending}>
            <Outlet />
          </RepoAccessBadgeContext.Provider>
        </ToastProvider>
      </div>
      <nav className="rail" aria-label="Primary">
        <div className="mark" title="Session Pods">
          <svg width="30" height="30" viewBox="0 0 30 30" fill="none">
            <path d="M15 2.5 26 9v12L15 27.5 4 21V9L15 2.5Z" fill="#FFB43A" />
            <path d="M15 9.5 20.5 12.7v5.6L15 21.5 9.5 18.3v-5.6L15 9.5Z" fill="#1c1404" />
            <circle cx="15" cy="15.5" r="2.1" fill="#FFB43A" />
          </svg>
        </div>
        <NavLink
          to="/"
          end
          className={({ isActive }) => "rail-btn" + (isActive ? " on" : "")}
          title="Sessions"
          aria-label="Sessions"
        >
          <svg width="20" height="20" viewBox="0 0 24 24" fill="none" stroke="currentColor" strokeWidth="2">
            <rect x="3" y="3" width="7" height="7" rx="1.5" />
            <rect x="14" y="3" width="7" height="7" rx="1.5" />
            <rect x="3" y="14" width="7" height="7" rx="1.5" />
            <rect x="14" y="14" width="7" height="7" rx="1.5" />
          </svg>
        </NavLink>
        <NavLink
          to={REPO_ACCESS_PATH}
          className={({ isActive }) => "rail-btn" + (isActive ? " on" : "")}
          title="Repo access"
          aria-label={pending ? `Repo access, ${pending} pending` : "Repo access"}
          data-testid="rail-repo-access"
        >
          <LockIcon />
          {pending ? (
            <span className="count" data-testid="repo-access-pending-count">
              {pending}
            </span>
          ) : null}
        </NavLink>
        <div className="rail-spacer" />
        <div className="rail-me" title="You">
          JD
        </div>
      </nav>
    </div>
  );
}
