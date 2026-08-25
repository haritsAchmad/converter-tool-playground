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
	RateRPS, RateBurst                                 float64
	MaxJobsPerIP                                       int
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
		RateRPS:         envFloat("CONVERTBOX_RATE_RPS", 1),
		RateBurst:       envFloat("CONVERTBOX_RATE_BURST", 5),
		MaxJobsPerIP:    envInt("CONVERTBOX_MAX_JOBS_PER_IP", 4),
	}
	if c.Workers < 1 || c.QueueSize < 1 || c.MaxUploadBytes < 1 || c.JobTTL < time.Minute {
		return c, fmt.Errorf("workers, queue, size must be positive and TTL at least 1m")
	}
	if c.RateRPS <= 0 || c.RateBurst < 1 || c.MaxJobsPerIP < 1 {
		return c, fmt.Errorf("rate rps/burst and max jobs per IP must be positive")
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
func envFloat(key string, fallback float64) float64 {
	v, err := strconv.ParseFloat(os.Getenv(key), 64)
	if err == nil && v > 0 {
		return v
	}
	return fallback
}
