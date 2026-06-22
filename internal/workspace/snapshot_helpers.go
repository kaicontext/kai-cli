package workspace

// GetSnapshotFileMap returns a map of path -> digest for a snapshot.
func (m *Manager) GetSnapshotFileMap(snapshotID []byte) (map[string]string, error) {
	return m.getSnapshotFileMap(snapshotID)
}
