import { NavLink, Outlet } from "react-router-dom";
import { ToastProvider } from "./Toast";
import "./shell.css";

// AppShell = main viewport + 64px bottom rail, ported from docs/mockups shell.
export function AppShell() {
  return (
    <div className="app">
      <div className="viewport">
        <ToastProvider>
          <Outlet />
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
        <div className="rail-spacer" />
        <div className="rail-me" title="You">
          JD
        </div>
      </nav>
    </div>
  );
}
