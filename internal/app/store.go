package app

import (
	"encoding/json"
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
)

const jobStateFile = "job.json"

// readJobState reads a job's job.json sidecar, retrying briefly on an error
// that isn't "the file doesn't exist". persist() writes via a temp file
// plus os.Rename, which is atomic on POSIX but can transiently fail a
// concurrent reader on Windows with a sharing violation while another
// process (antivirus, search indexing) has briefly opened the file being
// replaced—not a real "job not found", and without a retry it would wrongly
// 404 a live job's status/download mid-request. A genuinely missing file
// (wrong ID, already expired and removed) still fails immediately instead
// of paying the retry cost.
func readJobState(path string) ([]byte, error) {
	return readWithRetry(func() ([]byte, error) { return os.ReadFile(path) })
}

// readWithRetry runs read up to 3 times with a short delay between
// attempts, stopping as soon as it succeeds or fails with "not exist".
// Factored out of readJobState so the retry/backoff logic itself can be
// tested deterministically against a fake read function instead of trying
// to reproduce a real, OS-specific transient file error.
func readWithRetry(read func() ([]byte, error)) ([]byte, error) {
	var data []byte
	var err error
	for attempt := 0; attempt < 3; attempt++ {
		data, err = read()
		if err == nil || os.IsNotExist(err) {
			return data, err
		}
		time.Sleep(10 * time.Millisecond)
	}
	return data, err
}

type store struct {
	root string
	mu   sync.RWMutex
	jobs map[string]*Job
}

func newStore(root string) (*store, error) {
	abs, err := filepath.Abs(root)
	if err != nil {
		return nil, err
	}
	if err := os.MkdirAll(abs, 0700); err != nil {
		return nil, err
	}
	return &store{root: abs, jobs: make(map[string]*Job)}, nil
}
func (s *store) add(j *Job) { s.mu.Lock(); defer s.mu.Unlock(); s.jobs[j.ID] = j }

// persist writes the job's public state to a sidecar file next to its input,
// so status/download can be reported correctly after a restart.
func (s *store) persist(j *Job) error {
	dir := filepath.Dir(j.InputPath)
	data, err := json.Marshal(j.snapshot())
	if err != nil {
		return err
	}
	tmp := filepath.Join(dir, jobStateFile+".tmp")
	if err := os.WriteFile(tmp, data, 0600); err != nil {
		return err
	}
	return renameWithRetry(tmp, filepath.Join(dir, jobStateFile))
}

// renameWithRetry retries a same-directory os.Rename briefly on a
// transient error, the write-side counterpart to readJobState's own
// read-side retry. On Windows, os.Rename onto an existing destination
// needs to open that destination with delete access, which fails with
// "Access is denied" for as long as any other handle has it open for
// reading—which a concurrent status poll's readJobState (a plain
// os.ReadFile, open-read-close) does for a very short but nonzero
// window. Confirmed empirically, not assumed: instrumenting persist's
// os.Rename call and hammering a fast-completing job (SVG rasterizes in
// low single-digit milliseconds, so several persist() calls land close
// together while a 10ms-interval status poll is also reading the same
// file) reproduced "Access is denied" on effectively every run under
// `go test -race` (whose added scheduling overhead widens the
// collision window further). Without a retry, persist()'s caller in
// both worker() and process() only logs the failure and moves on—for
// process()'s FINAL Completed/Failed persist specifically, that also
// skips ackJob entirely (process returns false)—permanently stranding
// the job's on-disk state at whatever its last successful write was,
// most visibly "processing" forever, since every status read in this
// codebase (getJob and the worker's own dequeue) reloads from that
// file, never from the in-memory pointer a worker keeps mutating after
// persist() returns. 5 attempts at a 10ms backoff comfortably outlasts
// the reader's open-read-close window (microseconds in practice) while
// still failing fast on a genuine, non-transient error.
func renameWithRetry(oldpath, newpath string) error {
	var err error
	for attempt := 0; attempt < 5; attempt++ {
		if err = os.Rename(oldpath, newpath); err == nil {
			return nil
		}
		time.Sleep(10 * time.Millisecond)
	}
	return err
}

func (s *store) countActiveByIP(ip string) int {
	s.mu.RLock()
	ids := make([]string, 0, len(s.jobs))
	for id := range s.jobs {
		ids = append(ids, id)
	}
	s.mu.RUnlock()
	for _, id := range ids {
		_, _ = s.reload(id)
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	n := 0
	for _, j := range s.jobs {
		snap := j.snapshot()
		if snap.ClientIP == ip && (snap.Status == Queued || snap.Status == Processing) {
			n++
		}
	}
	return n
}

// recover rebuilds the in-memory job map from sidecar state files left by a
// previous process. Jobs still mid-flight at shutdown cannot be resumed, so
// they are marked failed; completed/failed jobs already on disk are restored
// as-is so their status and download remain available until they expire.
func (s *store) recover(now time.Time, log *slog.Logger, failQueued, failProcessing bool) {
	entries, err := os.ReadDir(s.root)
	if err != nil {
		return
	}
	for _, entry := range entries {
		if !entry.IsDir() || entry.Type()&os.ModeSymlink != 0 {
			continue
		}
		dir := filepath.Join(s.root, entry.Name())
		data, err := readJobState(filepath.Join(dir, jobStateFile))
		if err != nil {
			continue
		}
		var j Job
		if err := json.Unmarshal(data, &j); err != nil || j.ID != entry.Name() {
			continue
		}
		j.InputPath = filepath.Join(dir, "input.bin")
		if ext := filepath.Ext(j.OutputName); ext != "" {
			j.OutputPath = filepath.Join(dir, "output"+ext)
		}
		j.mu = &sync.RWMutex{}
		if now.After(j.ExpiresAt) {
			_ = os.RemoveAll(dir)
			continue
		}
		if (j.Status == Queued && failQueued) || (j.Status == Processing && failProcessing) {
			j.Status = Failed
			j.Error = "interrupted by server restart"
			finished := now
			j.FinishedAt = &finished
		}
		s.mu.Lock()
		s.jobs[j.ID] = &j
		s.mu.Unlock()
		if err := s.persist(&j); err != nil && log != nil {
			log.Warn("failed to persist recovered job state", "job_id", j.ID, "error", err)
		}
		if log != nil {
			log.Info("recovered job from disk", "job_id", j.ID, "status", j.Status)
		}
	}
}

// reload refreshes one job from the shared storage volume. Split API and
// worker processes do not share memory, so the sidecar is the source of truth.
func (s *store) reload(id string) (*Job, bool) {
	if _, err := uuid.Parse(id); err != nil || filepath.Base(id) != id {
		return nil, false
	}
	s.mu.RLock()
	existing := s.jobs[id]
	s.mu.RUnlock()
	dir := filepath.Join(s.root, id)
	data, err := readJobState(filepath.Join(dir, jobStateFile))
	if err != nil {
		return nil, false
	}
	var loaded Job
	if err := json.Unmarshal(data, &loaded); err != nil || loaded.ID != id {
		return nil, false
	}
	loaded.InputPath = filepath.Join(dir, "input.bin")
	ext := outputExtension(loaded.InputFormat, loaded.OutputFormat)
	if ext == "" {
		return nil, false
	}
	name := strings.ToLower(loaded.OutputName)
	matched := ext
	if !strings.HasSuffix(name, ext) {
		matched = ""
		// A legacy on-disk extension can only be trusted for a job that
		// already finished under the old convention: a real file was
		// written under that name and will never be rewritten. A job still
		// queued or processing has no output file yet, and the worker
		// always produces today's extension when it eventually runs, so its
		// recorded name is migrated to match instead of being chased under
		// a name the worker will never actually write (temuan review P2:
		// this previously accepted the legacy extension regardless of
		// status, so an old queued/processing PDF->image job kept an
		// "output.png"/"output.jpg" path while the worker wrote today's ZIP
		// bytes into it, producing a download that claimed to be an image
		// but wasn't one).
		if loaded.Status == Completed {
			for _, legacy := range legacyOutputExtensions(loaded.InputFormat, loaded.OutputFormat) {
				if strings.HasSuffix(name, legacy) {
					matched = legacy
					break
				}
			}
		}
		if matched == "" {
			if loaded.Status == Completed {
				return nil, false
			}
			matched = ext
			loaded.OutputName = strings.TrimSuffix(loaded.OutputName, filepath.Ext(loaded.OutputName)) + ext
		}
	}
	loaded.OutputPath = filepath.Join(dir, "output"+matched)
	loaded.mu = &sync.RWMutex{}
	if existing != nil {
		loaded.ClientIP = existing.snapshot().ClientIP
	}
	s.mu.Lock()
	s.jobs[id] = &loaded
	s.mu.Unlock()
	return &loaded, true
}
func (s *store) get(id string) (*Job, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	j, ok := s.jobs[id]
	return j, ok
}
func (s *store) remove(id string) error {
	s.mu.Lock()
	j, ok := s.jobs[id]
	if ok {
		delete(s.jobs, id)
	}
	s.mu.Unlock()
	if !ok {
		return nil
	}
	dir := filepath.Dir(j.InputPath)
	clean := filepath.Clean(dir)
	rel, err := filepath.Rel(s.root, clean)
	if err != nil || rel == "." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) || filepath.IsAbs(rel) {
		return errors.New("refusing unsafe cleanup path")
	}
	info, err := os.Lstat(clean)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	if info.Mode()&os.ModeSymlink != 0 {
		return errors.New("refusing symlink job directory")
	}
	return os.RemoveAll(clean)
}
func (s *store) cleanup(now time.Time) []string {
	s.mu.RLock()
	ids := make([]string, 0)
	for id, j := range s.jobs {
		if now.After(j.snapshot().ExpiresAt) {
			ids = append(ids, id)
		}
	}
	s.mu.RUnlock()
	removed := make([]string, 0, len(ids))
	for _, id := range ids {
		if s.remove(id) == nil {
			removed = append(removed, id)
		}
	}
	entries, _ := os.ReadDir(s.root)
	for _, entry := range entries {
		if !entry.IsDir() || entry.Type()&os.ModeSymlink != 0 {
			continue
		}
		s.mu.RLock()
		_, tracked := s.jobs[entry.Name()]
		s.mu.RUnlock()
		if tracked {
			continue
		}
		info, err := entry.Info()
		if err != nil || now.Sub(info.ModTime()) < time.Hour {
			continue
		}
		path := filepath.Join(s.root, entry.Name())
		rel, err := filepath.Rel(s.root, path)
		if err == nil && rel != "." && !filepath.IsAbs(rel) && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
			if os.RemoveAll(path) == nil {
				removed = append(removed, entry.Name())
			}
		}
	}
	return removed
}
