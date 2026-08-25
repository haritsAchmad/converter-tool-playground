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
)

const jobStateFile = "job.json"

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
	return os.Rename(tmp, filepath.Join(dir, jobStateFile))
}

func (s *store) countActiveByIP(ip string) int {
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
func (s *store) recover(now time.Time, log *slog.Logger) {
	entries, err := os.ReadDir(s.root)
	if err != nil {
		return
	}
	for _, entry := range entries {
		if !entry.IsDir() || entry.Type()&os.ModeSymlink != 0 {
			continue
		}
		dir := filepath.Join(s.root, entry.Name())
		data, err := os.ReadFile(filepath.Join(dir, jobStateFile))
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
		if j.Status == Queued || j.Status == Processing {
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
