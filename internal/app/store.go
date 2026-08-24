package app

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

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
