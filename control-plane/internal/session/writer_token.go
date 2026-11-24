// The credential that lets the control plane move a slot's writer seal, and
// nothing else in the pod do it.
//
// The seal routes live on the sidecar that mounts the slot's database, and the
// sidecar shares the pod's loopback with the workload container. Being in the
// pod therefore cannot be the authorisation: the session's own model reaches
// the same address the control plane reaches. So the sidecar requires a token
// it was given and the workload container was not — the orchestrator puts it on
// the sidecar only, exactly the way the pair's role already goes there, and the
// pod does not share a process namespace, so one container cannot read the
// other's environment.
//
// The token is derived rather than stored: HMAC of the session id under a key
// the control plane holds. That keeps it per session — a token lifted from one
// pod cannot seal another — with nothing new to persist, rotate or leak into a
// pod spec as a literal. A deployment with no key derives no token, and every
// call on this path is refused rather than allowed unauthenticated, which is
// what makes the pair's opt-in fail closed on an unprepared deployment.
package session

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"strings"
)

// writerTokenPurpose keeps this HMAC from colliding with any other use of the
// same key: the input is labelled, so a token minted here cannot be replayed as
// something else derived from the same secret.
const writerTokenPurpose = "session-platform/monitor-pair/slot-writer\x00"

// MonitorPairWriterToken is the token the sidecar of sessionID requires on the
// seal and monitor-write routes. An empty key answers empty, which every caller
// must read as "this deployment cannot hand work records over".
func MonitorPairWriterToken(key []byte, sessionID string) string {
	if len(key) == 0 || strings.TrimSpace(sessionID) == "" {
		return ""
	}
	mac := hmac.New(sha256.New, key)
	mac.Write([]byte(writerTokenPurpose))
	mac.Write([]byte(sessionID))
	return base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
}
