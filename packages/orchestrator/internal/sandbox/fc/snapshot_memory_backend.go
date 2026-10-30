package fc

type SnapshotMemoryBackendType uint8

const (
	SnapshotMemoryBackendUffd SnapshotMemoryBackendType = iota
	SnapshotMemoryBackendFile
)

type SnapshotMemoryBackend struct {
	Type       SnapshotMemoryBackendType
	Path       string
	Ready      <-chan struct{}
	TrackDirty bool
}

func NewUffdSnapshotMemoryBackend(path string, ready <-chan struct{}) SnapshotMemoryBackend {
	return SnapshotMemoryBackend{Type: SnapshotMemoryBackendUffd, Path: path, Ready: ready}
}

func NewFileSnapshotMemoryBackend(path string) SnapshotMemoryBackend {
	ready := make(chan struct{})
	close(ready)
	return SnapshotMemoryBackend{Type: SnapshotMemoryBackendFile, Path: path, Ready: ready}
}
