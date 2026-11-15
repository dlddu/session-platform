package session

import (
	"encoding/json"
	"fmt"
	"regexp"
	"time"
)

// ManifestVersion is the only manifest layout this control plane writes and reads.
const ManifestVersion = 1

// Manifest is the session record that outlives the session (AC-B4). It is
// written next to the archives on every committed snapshot, because the
// ConfigMap record is deleted with the session and the archive alone does not
// say which workload, model or tenant it belongs to.
type Manifest struct {
	Version         int          `json:"version"`
	ID              string       `json:"id"`
	Name            string       `json:"name"`
	WorkloadType    WorkloadType `json:"workloadType"`
	Model           string       `json:"model,omitempty"`
	Plugins         []string     `json:"plugins,omitempty"`
	ReasoningEffort string       `json:"reasoningEffort,omitempty"`
	Tenant          string       `json:"tenant,omitempty"`
	CreatedAt       time.Time    `json:"createdAt"`
	Checkpoint      Checkpoint   `json:"checkpoint"`
	// AutoArchive is the automatic archive the session's last pod was asked to
	// write when the session was deleted (AC-B5). It is newer than Checkpoint
	// when it exists.
	AutoArchive *Checkpoint `json:"autoArchive,omitempty"`
}

var sessionIDPattern = regexp.MustCompile(`^[0-9a-f]{32}$`)

// ValidSessionID reports whether id has the shape the control plane issues.
// The manifest key is built from the id, so anything else must never reach
// the object store.
func ValidSessionID(id string) bool {
	return sessionIDPattern.MatchString(id)
}

// ManifestFor captures sess with the archive it is being frozen into.
func ManifestFor(sess *Session, cp *Checkpoint) Manifest {
	m := Manifest{
		Version:         ManifestVersion,
		ID:              sess.ID,
		Name:            sess.Name,
		WorkloadType:    sess.WorkloadType,
		Model:           sess.Model,
		Plugins:         append([]string(nil), sess.Plugins...),
		ReasoningEffort: sess.ReasoningEffort,
		Tenant:          sess.Tenant,
		CreatedAt:       sess.CreatedAt,
	}
	if cp != nil {
		m.Checkpoint = *cp
		m.Checkpoint.AbortToken = ""
	}
	return m
}

// DecodeManifest parses a stored manifest and refuses one this control plane
// cannot faithfully turn back into a session.
func DecodeManifest(wire []byte) (*Manifest, error) {
	var m Manifest
	if err := json.Unmarshal(wire, &m); err != nil {
		return nil, fmt.Errorf("decode session manifest: %w", err)
	}
	if m.Version != ManifestVersion {
		return nil, fmt.Errorf("session manifest version %d is not supported", m.Version)
	}
	if !ValidSessionID(m.ID) || (m.Checkpoint.Ref == "" && (m.AutoArchive == nil || m.AutoArchive.Ref == "")) {
		return nil, fmt.Errorf("session manifest for %q is incomplete", m.ID)
	}
	return &m, nil
}

// SnapshotSession is the frozen session record a recovery puts back.
func (m *Manifest) SnapshotSession(now time.Time) *Session {
	cp := m.Checkpoint
	return &Session{
		ID:              m.ID,
		WorkloadType:    m.WorkloadType,
		Model:           m.Model,
		Plugins:         append([]string(nil), m.Plugins...),
		ReasoningEffort: m.ReasoningEffort,
		Tenant:          m.Tenant,
		Name:            m.Name,
		State:           StateSnapshot,
		CreatedAt:       m.CreatedAt,
		LastAccess:      now,
		Checkpoint:      &cp,
	}
}

// ArchiveKey is the session's one archive object, relative to the store's
// prefix. A snapshot and the pod's automatic archive both overwrite it (AC-B5).
func ArchiveKey(id string) string {
	return "sessions/" + id + "/checkpoint.tar"
}

// SnapshotWriter names a snapshot's archive by its generation.
func SnapshotWriter(generation string) string { return "generation:" + generation }

// PodWriter names a pod's automatic archive by the pod.
func PodWriter(pod string) string { return "pod:" + pod }
