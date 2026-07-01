module github.com/dlddu/session-platform/data-plane

go 1.24

// The agent is intentionally tiny: creack/pty attaches the session shell to a
// pseudo-terminal (AC-D1) and gorilla/websocket serves the /attach stream the
// control plane opens to prove reachability. Both are dependency-free.
require (
	github.com/creack/pty v1.1.24
	github.com/gorilla/websocket v1.5.3
)
