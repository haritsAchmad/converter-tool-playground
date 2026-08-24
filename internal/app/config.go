package app

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"time"
)

type Config struct {
	Address, StorageRoot                               string
	MaxUploadBytes                                     int64
	Workers, QueueSize                                 int
	JobTimeout, JobTTL, CleanupInterval, UploadTimeout time.Duration
}

func LoadConfig() (Config, error) {
	c := Config{
		Address:        env("CONVERTBOX_ADDR", ":8080"),
		StorageRoot:    env("CONVERTBOX_STORAGE", filepath.Join(os.TempDir(), "convertbox")),
		MaxUploadBytes: int64(envInt("CONVERTBOX_MAX_MB", 25)) << 20,
		Workers:        envInt("CONVERTBOX_WORKERS", 2), QueueSize: envInt("CONVERTBOX_QUEUE_SIZE", 20),
		JobTimeout:      envDuration("CONVERTBOX_JOB_TIMEOUT", 45*time.Second),
		JobTTL:          envDuration("CONVERTBOX_JOB_TTL", 20*time.Minute),
		CleanupInterval: envDuration("CONVERTBOX_CLEANUP_INTERVAL", time.Minute),
		UploadTimeout:   envDuration("CONVERTBOX_UPLOAD_TIMEOUT", 30*time.Second),
	}
	if c.Workers < 1 || c.QueueSize < 1 || c.MaxUploadBytes < 1 || c.JobTTL < time.Minute {
		return c, fmt.Errorf("workers, queue, size must be positive and TTL at least 1m")
	}
	return c, nil
}

func env(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}
func envInt(key string, fallback int) int {
	v, err := strconv.Atoi(os.Getenv(key))
	if err == nil && v > 0 {
		return v
	}
	return fallback
}
func envDuration(key string, fallback time.Duration) time.Duration {
	v, err := time.ParseDuration(os.Getenv(key))
	if err == nil && v > 0 {
		return v
	}
	return fallback
}
