package app

import (
	"sync"
	"time"
)

type Status string

const (
	Queued     Status = "queued"
	Processing Status = "processing"
	Completed  Status = "completed"
	Failed     Status = "failed"
)

type Job struct {
	ID           string     `json:"id"`
	Status       Status     `json:"status"`
	InputFormat  string     `json:"inputFormat"`
	OutputFormat string     `json:"outputFormat"`
	OriginalName string     `json:"originalName"`
	OutputName   string     `json:"outputName,omitempty"`
	Size         int64      `json:"size"`
	CreatedAt    time.Time  `json:"createdAt"`
	FinishedAt   *time.Time `json:"finishedAt,omitempty"`
	ExpiresAt    time.Time  `json:"expiresAt"`
	Error        string     `json:"error,omitempty"`
	InputPath    string     `json:"-"`
	OutputPath   string     `json:"-"`
	ClientIP     string     `json:"-"`
	mu           *sync.RWMutex
}

func (j *Job) snapshot() Job {
	j.mu.RLock()
	defer j.mu.RUnlock()
	c := *j
	c.mu = nil
	return c
}
func (j *Job) update(fn func(*Job)) { j.mu.Lock(); defer j.mu.Unlock(); fn(j) }

type Format struct {
	ID, Label, Group string
	Extensions       []string
}
type publicFormat struct {
	ID         string   `json:"id"`
	Label      string   `json:"label"`
	Group      string   `json:"group"`
	Extensions []string `json:"extensions"`
	Outputs    []string `json:"outputs"`
}
