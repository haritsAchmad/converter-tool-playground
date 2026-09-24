package app

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// TestValidateVideoStreams is a table of validateVideo's actual
// accept/reject decision, exercised as a pure function against
// hand-written ffprobe-shaped fixtures rather than a real ffprobe binary:
// codec whitelist enforcement per container, the resolution/duration
// bounds, silent (video-only) input being valid, and--unlike audio, which
// has the attached-picture exception--any extra stream of any kind being
// rejected outright.
func TestValidateVideoStreams(t *testing.T) {
	videoStream := func(codec string, w, h int) ffprobeStream {
		return ffprobeStream{CodecName: codec, CodecType: "video", Width: w, Height: h}
	}
	audioStream := func(codec string) ffprobeStream {
		return ffprobeStream{CodecName: codec, CodecType: "audio"}
	}
	attachedPic := ffprobeStream{CodecName: "mjpeg", CodecType: "video", Disposition: map[string]int{"attached_pic": 1}}
	subtitle := ffprobeStream{CodecName: "subrip", CodecType: "subtitle"}

	cases := []struct {
		name    string
		format  string
		streams []ffprobeStream
		dur     string
		rejects bool
	}{
		{"plain mp4 (h264+aac)", "mp4", []ffprobeStream{videoStream("h264", 1280, 720), audioStream("aac")}, "30", false},
		{"silent mp4 (video only)", "mp4", []ffprobeStream{videoStream("h264", 640, 480)}, "10", false},
		{"webm vp8+vorbis", "webm", []ffprobeStream{videoStream("vp8", 640, 360), audioStream("vorbis")}, "10", false},
		{"webm vp9+opus", "webm", []ffprobeStream{videoStream("vp9", 1280, 720), audioStream("opus")}, "10", false},
		{"exactly at the pixel ceiling", "mp4", []ffprobeStream{videoStream("h264", 1280, 720)}, "1", false},
		{"wrong video codec for container", "mp4", []ffprobeStream{videoStream("vp9", 640, 480)}, "10", true},
		{"wrong audio codec for container", "mp4", []ffprobeStream{videoStream("h264", 640, 480), audioStream("opus")}, "10", true},
		{"over the pixel ceiling", "mp4", []ffprobeStream{videoStream("h264", 1920, 1080)}, "10", true},
		{"zero dimensions", "mp4", []ffprobeStream{videoStream("h264", 0, 0)}, "10", true},
		{"two video streams", "mp4", []ffprobeStream{videoStream("h264", 640, 480), videoStream("h264", 640, 480)}, "10", true},
		{"two audio streams", "mp4", []ffprobeStream{videoStream("h264", 640, 480), audioStream("aac"), audioStream("aac")}, "10", true},
		{"no video stream at all", "mp4", []ffprobeStream{audioStream("aac")}, "10", true},
		{"attached-picture stream not exempt (unlike audio)", "mp4", []ffprobeStream{videoStream("h264", 640, 480), attachedPic}, "10", true},
		{"subtitle stream", "mp4", []ffprobeStream{videoStream("h264", 640, 480), subtitle}, "10", true},
		{"duration exceeds limit", "mp4", []ffprobeStream{videoStream("h264", 640, 480)}, "99999", true},
		{"zero duration", "mp4", []ffprobeStream{videoStream("h264", 640, 480)}, "0", true},
		{"unparseable duration", "mp4", []ffprobeStream{videoStream("h264", 640, 480)}, "not-a-number", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			probe := ffprobeOutput{Streams: tc.streams, Format: ffprobeFormat{Duration: tc.dur}}
			err := validateVideoStreams(tc.format, probe)
			if tc.rejects && err == nil {
				t.Fatalf("expected rejection, got none")
			}
			if !tc.rejects && err != nil {
				t.Fatalf("expected acceptance, got %v", err)
			}
		})
	}
}

func TestValidateUploadRejectsWrongVideoSignature(t *testing.T) {
	for _, tc := range []struct{ format, ext string }{
		{"mp4", ".mp4"}, {"webm", ".webm"},
	} {
		t.Run(tc.format, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "input.bin")
			if err := os.WriteFile(path, []byte("this is not a real video file at all"), 0600); err != nil {
				t.Fatal(err)
			}
			if _, err := validateUpload(path, "clip"+tc.ext); err == nil {
				t.Fatalf("expected a file with the wrong %s signature to be rejected", tc.format)
			}
		})
	}
}

func TestVideoCapabilitiesDependOnFFmpeg(t *testing.T) {
	c := &converter{}
	if c.supports("mp4", "webm") {
		t.Fatal("video conversion advertised without ffmpeg/ffprobe")
	}
	c.ffmpeg = filepath.Join("tools", "ffmpeg")
	if c.supports("mp4", "webm") {
		t.Fatal("video conversion advertised with only ffmpeg, no ffprobe")
	}
	c.ffprobe = filepath.Join("tools", "ffprobe")
	if !c.supports("mp4", "webm") {
		t.Fatal("expected mp4 -> webm support with ffmpeg/ffprobe")
	}
	if !c.supports("webm", "mp4") {
		t.Fatal("expected webm -> mp4 support with ffmpeg/ffprobe")
	}
	if c.supports("mp4", "mp4") {
		t.Fatal("mp4 -> mp4 must not be advertised")
	}
	if c.supports("mp4", "mp3") {
		t.Fatal("mp4 -> mp3 (video into an audio-only container) must not be advertised in this slice")
	}
}

func TestConvertVideoReachesFFmpeg(t *testing.T) {
	c := fakeFFmpegConverter(t)
	inPath := filepath.Join(t.TempDir(), "input.bin")
	if err := os.WriteFile(inPath, []byte{0x1A, 0x45, 0xDF, 0xA3}, 0600); err != nil {
		t.Fatal(err)
	}
	outPath := filepath.Join(t.TempDir(), "output.mp4")
	err := c.run(context.Background(), "webm", "mp4", "", inPath, outPath)
	if err == nil {
		t.Fatal("expected an error from the fake ffmpeg binary")
	}
	if !strings.Contains(err.Error(), "video conversion failed") {
		t.Fatalf("expected the conversion to reach ffmpeg and fail there, got %v", err)
	}
}

// tinyVideo generates a real, tiny synthetic video fixture (a solid test
// pattern, optionally with a sine-wave audio track) via ffmpeg's own lavfi
// test sources, the same technique used to prototype this feature's
// ffmpeg command lines before writing any Go code. There's no practical
// way to hand-build a minimal-but-valid MP4/WebM fixture byte-by-byte the
// way tinyPNG/tinyWAV do for their much simpler formats, so this only
// runs where ffmpeg itself is available--callers must call
// ffmpegLookPath(t) first.
func tinyVideo(t *testing.T, dir, format string, seconds float64, withAudio bool) string {
	t.Helper()
	path := filepath.Join(dir, "fixture."+format)
	args := []string{"-nostdin", "-v", "error", "-f", "lavfi", "-i", "testsrc=duration=0.5:size=320x240:rate=10"}
	if withAudio {
		args = append(args, "-f", "lavfi", "-i", "sine=frequency=1000:duration=0.5")
	}
	args = append(args, "-t", "0.5")
	args = append(args, videoOutputEncoder[format]...)
	args = append(args, "-f", format, path)
	out, err := exec.Command("ffmpeg", args...).CombinedOutput()
	if err != nil {
		t.Fatalf("could not build video fixture: %v (%s)", err, out)
	}
	return path
}

// TestValidateVideoAcceptsRealFixtures is the real end-to-end validation
// case: genuine MP4 and WebM files, generated by ffmpeg itself, must pass
// validateUpload's full pipeline (magic bytes through ffprobe).
func TestValidateVideoAcceptsRealFixtures(t *testing.T) {
	ffmpegLookPath(t)
	for _, format := range []string{"mp4", "webm"} {
		t.Run(format, func(t *testing.T) {
			path := tinyVideo(t, t.TempDir(), format, 0.5, true)
			if _, err := validateUpload(path, "clip."+format); err != nil {
				t.Fatalf("expected a real %s fixture to validate, got: %v", format, err)
			}
		})
	}
}

// TestConvertVideoEndToEnd is the real end-to-end conversion case, skipped
// where ffmpeg/ffprobe aren't installed, same as the audio/LibreOffice/
// poppler-dependent tests.
func TestConvertVideoEndToEnd(t *testing.T) {
	ffmpegLookPath(t)
	c := newConverter()
	for _, tc := range []struct{ in, out string }{{"mp4", "webm"}, {"webm", "mp4"}} {
		t.Run(tc.in+"_to_"+tc.out, func(t *testing.T) {
			dir := t.TempDir()
			inPath := tinyVideo(t, dir, tc.in, 0.5, true)
			outPath := filepath.Join(dir, "output."+tc.out)
			if err := c.run(context.Background(), tc.in, tc.out, "", inPath, outPath); err != nil {
				t.Fatalf("%s -> %s conversion failed: %v", tc.in, tc.out, err)
			}
			info, err := os.Stat(outPath)
			if err != nil || info.Size() == 0 {
				t.Fatalf("expected a non-empty output file, err=%v", err)
			}
			if err := validateVideo(tc.out, outPath); err != nil {
				t.Fatalf("converted output failed its own video validation: %v", err)
			}
		})
	}
}

// TestVideoJobFlowsThroughWorker is the full-pipeline regression, mirroring
// TestAudioJobFlowsThroughWorker: submit a real video job over HTTP and let
// the actual worker goroutine dequeue and convert it exactly as production
// does. Skip-guarded like the other real-tool tests, since validateVideo
// needs a real ffprobe at upload time, not just at conversion time.
func TestVideoJobFlowsThroughWorker(t *testing.T) {
	ffmpegLookPath(t)
	a := testApp(t)

	fixturePath := tinyVideo(t, t.TempDir(), "mp4", 0.5, true)
	body, err := os.ReadFile(fixturePath)
	if err != nil {
		t.Fatal(err)
	}
	w := submitJob(t, a, "clip.mp4", body, map[string]string{"outputFormat": "webm"})
	if w.Code != http.StatusAccepted {
		t.Fatalf("expected 202, got %d: %s", w.Code, w.Body.String())
	}
	var created Job
	if err := json.Unmarshal(w.Body.Bytes(), &created); err != nil {
		t.Fatal(err)
	}

	deadline := time.Now().Add(5 * time.Second)
	var job Job
	for time.Now().Before(deadline) {
		r := httptest.NewRequest(http.MethodGet, "/api/v1/jobs/"+created.ID, nil)
		rec := httptest.NewRecorder()
		a.Handler().ServeHTTP(rec, r)
		if rec.Code != http.StatusOK {
			t.Fatalf("status endpoint returned %d for a live job", rec.Code)
		}
		_ = json.Unmarshal(rec.Body.Bytes(), &job)
		if job.Status == Completed || job.Status == Failed {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if job.Status != Completed {
		t.Fatalf("expected the MP4 -> WebM job to complete, got %s (error: %q)", job.Status, job.Error)
	}
}
