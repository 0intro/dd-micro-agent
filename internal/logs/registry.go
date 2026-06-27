package logs

import (
	"context"
	"encoding/json"
	"log/slog"
	"os"
	"path/filepath"
	"strconv"
	"sync"
	"time"
)

// Registry remembers how far each file has been read and successfully sent, so a
// restart resumes instead of re-sending everything. Offsets only ever move
// forward, and the file is written atomically. The contract is at-least-once: a
// crash between a send and the next persist re-reads, never loses.
type Registry struct {
	path string
	log  *slog.Logger

	mu      sync.Mutex
	offsets map[string]int64  // file identifier -> byte offset
	cursors map[string]string // journald identifier -> opaque cursor
}

const registryVersion = 2

type registryFile struct {
	Version  int                       `json:"Version"`
	Registry map[string]registryRecord `json:"Registry"`
}

// registryRecord holds either a file byte offset or a journald cursor for an id,
// never both: a given source is one kind. Both are omitempty so a record carries
// only its own field.
type registryRecord struct {
	Offset string `json:"Offset,omitempty"`
	Cursor string `json:"Cursor,omitempty"`
}

// LoadRegistry reads the registry at path. A missing or corrupt file yields an
// empty registry (the agent simply re-reads from the start).
func LoadRegistry(path string, log *slog.Logger) *Registry {
	if log == nil {
		log = slog.Default()
	}
	r := &Registry{path: path, log: log, offsets: make(map[string]int64), cursors: make(map[string]string)}

	data, err := os.ReadFile(path)
	if err != nil {
		return r
	}
	var rf registryFile
	if err := json.Unmarshal(data, &rf); err != nil {
		log.Warn("registry corrupt, starting fresh", "path", path, "err", err)
		return r
	}
	for id, rec := range rf.Registry {
		if rec.Cursor != "" {
			r.cursors[id] = rec.Cursor
			continue
		}
		if n, err := strconv.ParseInt(rec.Offset, 10, 64); err == nil {
			r.offsets[id] = n
		}
	}
	return r
}

// Offset returns the stored offset for id, or 0 if none.
func (r *Registry) Offset(id string) int64 {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.offsets[id]
}

// Position returns the stored offset for id and whether an entry exists. A file
// the registry has never seen is new to us, so the tailer starts it at the end
// rather than replaying pre-existing history.
func (r *Registry) Position(id string) (int64, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	off, ok := r.offsets[id]
	return off, ok
}

// Advance records id's delivered offset. The single batcher delivers in order,
// so the last write is always the furthest delivered byte, and a rotation
// legitimately rewinds it: the tailer restarts the new file at zero, and holding
// the old file's larger offset would make a restart skip bytes that were never
// delivered.
func (r *Registry) Advance(id string, off int64) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.offsets[id] = off
}

// Remove forgets id, for a source that no longer exists. A late
// acknowledgement may re-create the entry, which is harmless: the reopen
// guards handle a stale offset.
func (r *Registry) Remove(id string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	delete(r.offsets, id)
	delete(r.cursors, id)
}

// PositionCursor returns the stored journald cursor for id and whether one
// exists. A journald source we have never seen has no cursor, so its tailer
// starts per its start_position.
func (r *Registry) PositionCursor(id string) (string, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	c, ok := r.cursors[id]
	return c, ok
}

// AdvanceCursor records id's journald cursor. The single batcher delivers in
// order, so the last write is the furthest read. Unlike byte offsets, journald
// cursors are opaque and not comparable, so this just keeps the latest.
func (r *Registry) AdvanceCursor(id, cursor string) {
	if cursor == "" {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.cursors[id] = cursor
}

// Run flushes the registry to disk every interval and once more when ctx ends.
func (r *Registry) Run(ctx context.Context, interval time.Duration) {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			r.Flush()
			return
		case <-ticker.C:
			r.Flush()
		}
	}
}

// Flush writes the registry atomically: a temp file in the same directory is
// renamed over the target, so a reader never sees a half-written file.
func (r *Registry) Flush() {
	r.mu.Lock()
	rf := registryFile{Version: registryVersion, Registry: make(map[string]registryRecord, len(r.offsets)+len(r.cursors))}
	for id, off := range r.offsets {
		rf.Registry[id] = registryRecord{Offset: strconv.FormatInt(off, 10)}
	}
	for id, cur := range r.cursors {
		rf.Registry[id] = registryRecord{Cursor: cur}
	}
	r.mu.Unlock()

	data, err := json.Marshal(rf)
	if err != nil {
		r.log.Warn("registry marshal failed", "err", err)
		return
	}
	if err := os.MkdirAll(filepath.Dir(r.path), 0o755); err != nil {
		r.log.Warn("registry mkdir failed", "err", err)
		return
	}
	tmp := r.path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		r.log.Warn("registry write failed", "err", err)
		return
	}
	if err := os.Rename(tmp, r.path); err != nil {
		r.log.Warn("registry rename failed", "err", err)
	}
}
