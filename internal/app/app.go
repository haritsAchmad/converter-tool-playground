package app

import (
	"context"
	"embed"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"mime/multipart"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/collectors"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"go.opentelemetry.io/contrib/instrumentation/net/http/otelhttp"
)

//go:embed web/*
var webFiles embed.FS

type App struct {
	cfg       Config
	log       *slog.Logger
	store     *store
	converter *converter
	queue     chan *Job
	limiter   *rateLimiter
	metrics   *metrics
	registry  *prometheus.Registry
	scanner   malwareScanner
	ctx       context.Context
	cancel    context.CancelFunc
	wg        sync.WaitGroup
}

func New(cfg Config, logger *slog.Logger) (*App, error) {
	s, err := newStore(cfg.StorageRoot)
	if err != nil {
		return nil, err
	}
	scanner, err := newClamScanner(cfg.ClamScanPath)
	if err != nil {
		return nil, err
	}
	ctx, cancel := context.WithCancel(context.Background())
	queue := make(chan *Job, cfg.QueueSize)
	registry := prometheus.NewRegistry()
	registry.MustRegister(collectors.NewGoCollector(), collectors.NewProcessCollector(collectors.ProcessCollectorOpts{}))
	m := newMetrics(registry, func() float64 { return float64(len(queue)) })
	a := &App{
		cfg: cfg, log: logger, store: s, converter: newConverter(),
		queue: queue, limiter: newRateLimiter(cfg.RateRPS, cfg.RateBurst),
		metrics: m, registry: registry, scanner: scanner, ctx: ctx, cancel: cancel,
	}
	s.recover(time.Now().UTC(), logger)
	for i := 0; i < cfg.Workers; i++ {
		a.wg.Add(1)
		go a.worker(i)
	}
	a.wg.Add(1)
	go a.janitor()
	return a, nil
}
func (a *App) Close() { a.cancel(); a.wg.Wait() }

func (a *App) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusNoContent) })
	mux.HandleFunc("GET /api/v1/formats", a.getFormats)
	mux.Handle("POST /api/v1/jobs", a.rateLimitJobs(a.createJob))
	mux.HandleFunc("GET /api/v1/jobs/{id}", a.getJob)
	mux.HandleFunc("GET /api/v1/jobs/{id}/download", a.download)
	mux.Handle("GET /metrics", promhttp.HandlerFor(a.registry, promhttp.HandlerOpts{}))
	static, _ := fs.Sub(webFiles, "web")
	mux.Handle("/", http.FileServer(http.FS(static)))

	// otelhttp is wired against the global (no-op by default) TracerProvider,
	// so tracing lights up automatically if the process later configures a
	// real exporter, at negligible cost while none is configured.
	traced := otelhttp.NewHandler(mux, "convertbox", otelhttp.WithSpanNameFormatter(func(_ string, r *http.Request) string {
		return r.Method + " " + metricsPath(r.URL.Path)
	}))
	return a.securityHeaders(a.recoverer(withRequestID(a.accessLog(traced))))
}

func (a *App) getFormats(w http.ResponseWriter, r *http.Request) {
	list := a.converter.capabilities()
	sort.Slice(list, func(i, j int) bool { return list[i].ID < list[j].ID })
	writeJSON(w, http.StatusOK, map[string]any{"formats": list, "maxUploadBytes": a.cfg.MaxUploadBytes, "jobTTLSeconds": int(a.cfg.JobTTL.Seconds())})
}

func (a *App) createJob(w http.ResponseWriter, r *http.Request) {
	ip := clientIP(r)
	if a.store.countActiveByIP(ip) >= a.cfg.MaxJobsPerIP {
		writeError(w, http.StatusTooManyRequests, "too many concurrent jobs for this client")
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, a.cfg.MaxUploadBytes+(1<<20))
	mr, err := r.MultipartReader()
	if err != nil {
		writeError(w, http.StatusBadRequest, "expected multipart/form-data")
		return
	}
	id := uuid.NewString()
	jobDir := filepath.Join(a.store.root, id)
	if err := os.Mkdir(jobDir, 0700); err != nil {
		writeError(w, 500, "could not allocate job")
		return
	}
	keep := false
	defer func() {
		if !keep {
			_ = os.RemoveAll(jobDir)
		}
	}()
	fields := map[string]string{}
	var original, inputPath string
	var size int64
	for {
		part, err := mr.NextPart()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			writeError(w, 400, "invalid multipart body")
			return
		}
		if part.FormName() == "file" && original == "" {
			original = filepath.Base(part.FileName())
			inputPath = filepath.Join(jobDir, "input.bin")
			size, err = saveUpload(part, inputPath, a.cfg.MaxUploadBytes)
			_ = part.Close()
			if err != nil {
				writeError(w, 400, err.Error())
				return
			}
		} else {
			b, readErr := io.ReadAll(io.LimitReader(part, 257))
			_ = part.Close()
			if readErr != nil || len(b) > 256 {
				writeError(w, 400, "invalid form field")
				return
			}
			fields[part.FormName()] = string(b)
		}
	}
	if inputPath == "" {
		writeError(w, 400, "file is required")
		return
	}
	in, err := validateUpload(inputPath, original)
	if err != nil {
		writeError(w, 415, err.Error())
		return
	}
	if a.scanner != nil {
		scanCtx, cancel := context.WithTimeout(r.Context(), a.cfg.ScanTimeout)
		err := a.scanner.Scan(scanCtx, inputPath)
		cancel()
		if errors.Is(err, errMalwareDetected) {
			writeError(w, http.StatusUnprocessableEntity, "file failed malware scan")
			return
		}
		if err != nil {
			a.log.Warn("malware scan failed", "error", err)
			writeError(w, http.StatusServiceUnavailable, "malware scanner unavailable")
			return
		}
	}
	out := strings.ToLower(strings.TrimSpace(fields["outputFormat"]))
	if !a.converter.supports(in, out) {
		writeError(w, 400, "unsupported input/output format pair")
		return
	}
	base := sanitizeBase(fields["outputName"])
	if base == "" {
		base = strings.TrimSuffix(original, filepath.Ext(original))
	}
	base = sanitizeBase(base)
	if base == "" {
		base = "converted"
	}
	ext := formats[out].Extensions[0]
	outPath := filepath.Join(jobDir, "output"+ext)
	if !within(jobDir, inputPath) || !within(jobDir, outPath) {
		writeError(w, 400, "unsafe path")
		return
	}
	now := time.Now().UTC()
	j := &Job{ID: id, Status: Queued, InputFormat: in, OutputFormat: out, OriginalName: original, OutputName: base + ext, Size: size, CreatedAt: now, ExpiresAt: now.Add(a.cfg.JobTTL), InputPath: inputPath, OutputPath: outPath, ClientIP: ip, mu: &sync.RWMutex{}}
	a.store.add(j)
	if err := a.store.persist(j); err != nil {
		a.log.Warn("failed to persist job state", "job_id", j.ID, "error", err)
	}
	select {
	case a.queue <- j:
		keep = true
		writeJSON(w, http.StatusAccepted, j.snapshot())
	default:
		_ = a.store.remove(id)
		writeError(w, http.StatusServiceUnavailable, "conversion queue is full")
	}
}

func saveUpload(part *multipart.Part, path string, max int64) (int64, error) {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if err != nil {
		return 0, err
	}
	n, copyErr := io.Copy(f, io.LimitReader(part, max+1))
	closeErr := f.Close()
	if copyErr != nil {
		return n, copyErr
	}
	if closeErr != nil {
		return n, closeErr
	}
	if n == 0 {
		return 0, errors.New("empty files are not accepted")
	}
	if n > max {
		return n, fmt.Errorf("file exceeds %d byte limit", max)
	}
	return n, nil
}

func (a *App) getJob(w http.ResponseWriter, r *http.Request) {
	j, ok := a.validJob(r.PathValue("id"))
	if !ok {
		writeError(w, 404, "job not found or expired")
		return
	}
	writeJSON(w, 200, j.snapshot())
}
func (a *App) download(w http.ResponseWriter, r *http.Request) {
	j, ok := a.validJob(r.PathValue("id"))
	if !ok {
		writeError(w, 404, "job not found or expired")
		return
	}
	snap := j.snapshot()
	if snap.Status != Completed {
		writeError(w, 409, "job is not complete")
		return
	}
	if !within(filepath.Dir(snap.InputPath), snap.OutputPath) {
		writeError(w, 500, "invalid output path")
		return
	}
	info, err := os.Lstat(snap.OutputPath)
	if err != nil || !info.Mode().IsRegular() {
		writeError(w, 404, "output is unavailable")
		return
	}
	f, err := os.Open(snap.OutputPath)
	if err != nil {
		writeError(w, 404, "output is unavailable")
		return
	}
	defer f.Close()
	w.Header().Set("Content-Disposition", fmt.Sprintf("attachment; filename=%q", snap.OutputName))
	w.Header().Set("Content-Type", "application/octet-stream")
	http.ServeContent(w, r, snap.OutputName, info.ModTime(), f)
}
func (a *App) validJob(id string) (*Job, bool) {
	if _, err := uuid.Parse(id); err != nil {
		return nil, false
	}
	j, ok := a.store.get(id)
	if !ok {
		return nil, false
	}
	if time.Now().After(j.snapshot().ExpiresAt) {
		_ = a.store.remove(id)
		return nil, false
	}
	return j, true
}

func (a *App) worker(index int) {
	defer a.wg.Done()
	for {
		select {
		case <-a.ctx.Done():
			return
		case j := <-a.queue:
			a.process(index, j)
		}
	}
}
func (a *App) process(index int, j *Job) {
	start := time.Now()
	j.update(func(x *Job) { x.Status = Processing })
	ctx, cancel := context.WithTimeout(a.ctx, a.cfg.JobTimeout)
	defer cancel()
	err := a.converter.run(ctx, j.InputFormat, j.OutputFormat, j.InputPath, j.OutputPath)
	now := time.Now().UTC()
	j.update(func(x *Job) {
		x.FinishedAt = &now
		x.ExpiresAt = now.Add(a.cfg.JobTTL)
		if err != nil {
			x.Status = Failed
			x.Error = "conversion failed"
		} else {
			x.Status = Completed
		}
	})
	if perr := a.store.persist(j); perr != nil {
		a.log.Warn("failed to persist job state", "job_id", j.ID, "error", perr)
	}
	a.metrics.jobsTotal.WithLabelValues(string(j.snapshot().Status), j.InputFormat, j.OutputFormat).Inc()
	a.metrics.jobDuration.Observe(time.Since(start).Seconds())
	if err != nil {
		_ = os.Remove(j.OutputPath)
		a.log.Warn("conversion failed", "job_id", j.ID, "input", j.InputFormat, "output", j.OutputFormat, "size", j.Size, "worker", index, "error", err)
	} else {
		a.log.Info("conversion completed", "job_id", j.ID, "input", j.InputFormat, "output", j.OutputFormat, "size", j.Size, "worker", index)
	}
}
func (a *App) janitor() {
	defer a.wg.Done()
	ticker := time.NewTicker(a.cfg.CleanupInterval)
	defer ticker.Stop()
	for {
		select {
		case <-a.ctx.Done():
			return
		case now := <-ticker.C:
			for _, id := range a.store.cleanup(now) {
				a.log.Info("expired job removed", "job_id", id)
			}
			a.limiter.sweep(30 * time.Minute)
		}
	}
}

func (a *App) securityHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("X-Frame-Options", "DENY")
		w.Header().Set("Referrer-Policy", "no-referrer")
		w.Header().Set("Content-Security-Policy", "default-src 'self'; style-src 'self'; script-src 'self'; img-src 'self' data:; connect-src 'self'")
		w.Header().Set("Permissions-Policy", "camera=(), microphone=(), geolocation=()")
		next.ServeHTTP(w, r)
	})
}
func (a *App) recoverer(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() {
			if v := recover(); v != nil {
				a.log.Error("panic recovered", "error", fmt.Sprint(v))
				writeError(w, 500, "internal server error")
			}
		}()
		next.ServeHTTP(w, r)
	})
}
func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}
func writeError(w http.ResponseWriter, status int, message string) {
	writeJSON(w, status, map[string]string{"error": message})
}

var safeBase = regexp.MustCompile(`[^a-zA-Z0-9._ -]+`)

func sanitizeBase(s string) string {
	s = filepath.Base(strings.TrimSpace(s))
	s = strings.Trim(s, ". ")
	s = safeBase.ReplaceAllString(s, "_")
	if len(s) > 80 {
		s = s[:80]
	}
	return s
}
func within(root, path string) bool {
	rel, err := filepath.Rel(filepath.Clean(root), filepath.Clean(path))
	return err == nil && rel != ".." && !filepath.IsAbs(rel) && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}
