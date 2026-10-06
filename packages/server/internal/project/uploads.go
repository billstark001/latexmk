package project

import (
	"errors"
	"fmt"
	"time"

	"github.com/billstark001/latexmk/packages/server/internal/api"
)

// PrepareUpload pins a verified immutable manifest without consuming its upload.
// The caller transfers this pin to a job, or releases it on admission failure.
// Keeping the upload until admission succeeds makes failed/retried commits safe.
func (m *Manager) PrepareUpload(ownerID, uploadID string) (Snapshot, api.CompileRequest, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	s, ok := m.sessions[uploadID]
	if !ok || s.ownerID != ownerID || time.Now().After(s.expires) || s.committing {
		return Snapshot{}, api.CompileRequest{}, errors.New("upload session not found or expired")
	}
	for digest, size := range s.expected {
		if !m.hasBlob(ownerID, digest, size) {
			return Snapshot{}, api.CompileRequest{}, fmt.Errorf("missing required digest %s", digest)
		}
	}
	snapshot, err := NewSnapshot(ownerID, s.projectID, s.files)
	if err != nil {
		return Snapshot{}, api.CompileRequest{}, err
	}
	pin := m.pins[snapshot.ID]
	pin.Snapshot = snapshot
	pin.Count++
	m.pins[snapshot.ID] = pin
	return snapshot, s.request, nil
}

func (m *Manager) ConsumeUpload(ownerID, uploadID string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if s, ok := m.sessions[uploadID]; ok && s.ownerID == ownerID {
		delete(m.sessions, uploadID)
	}
}
