package app

import (
	"bytes"
	"context"
	"encoding/binary"
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

// tinyWAV returns a minimal, valid mono 16-bit PCM WAV of the given
// duration (silence), hand-built from the standard RIFF/WAVE chunk layout
// rather than needing ffmpeg to generate a fixture, the same spirit as
// tinyPNG for image fixtures.
func tinyWAV(t *testing.T, seconds float64) []byte {
	t.Helper()
	const sampleRate = 8000
	numSamples := int(seconds * sampleRate)
	if numSamples < 1 {
		numSamples = 1
	}
	dataSize := numSamples * 2 // 16-bit mono
	var buf bytes.Buffer
	buf.WriteString("RIFF")
	_ = binary.Write(&buf, binary.LittleEndian, uint32(36+dataSize))
	buf.WriteString("WAVE")
	buf.WriteString("fmt ")
	_ = binary.Write(&buf, binary.LittleEndian, uint32(16))
	_ = binary.Write(&buf, binary.LittleEndian, uint16(1)) // PCM
	_ = binary.Write(&buf, binary.LittleEndian, uint16(1)) // mono
	_ = binary.Write(&buf, binary.LittleEndian, uint32(sampleRate))
	_ = binary.Write(&buf, binary.LittleEndian, uint32(sampleRate*2))
	_ = binary.Write(&buf, binary.LittleEndian, uint16(2))
	_ = binary.Write(&buf, binary.LittleEndian, uint16(16))
	buf.WriteString("data")
	_ = binary.Write(&buf, binary.LittleEndian, uint32(dataSize))
	buf.Write(make([]byte, dataSize))
	return buf.Bytes()
}

func TestIsMP3Signature(t *testing.T) {
	cases := []struct {
		name string
		head []byte
		want bool
	}{
		{"ID3 tag", []byte("ID3\x03\x00\x00\x00\x00\x00\x00"), true},
		{"raw frame sync", []byte{0xFF, 0xFB, 0x90, 0x00}, true},
		{"frame sync with lower bits set", []byte{0xFF, 0xE0}, true},
		{"not mp3", []byte("RIFF....WAVE"), false},
		{"too short", []byte{0xFF}, false},
		{"FF without sync bits", []byte{0xFF, 0x00}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := isMP3Signature(tc.head); got != tc.want {
				t.Fatalf("isMP3Signature(%v) = %v, want %v", tc.head, got, tc.want)
			}
		})
	}
}

// TestValidateAudioStreams is a table of validateAudio's actual accept/
// reject decision, exercised as a pure function against hand-written
// ffprobe-shaped fixtures rather than a real ffprobe binary (unavailable
// on this machine): codec whitelist enforcement, the attached-picture
// cover-art allowance, rejection of a real video/subtitle/data stream or
// more than one audio stream, and the duration bound.
func TestValidateAudioStreams(t *testing.T) {
	audioStream := func(codec string) ffprobeStream {
		return ffprobeStream{CodecName: codec, CodecType: "audio"}
	}
	attachedPic := ffprobeStream{CodecName: "mjpeg", CodecType: "video", Disposition: map[string]int{"attached_pic": 1}}
	realVideo := ffprobeStream{CodecName: "h264", CodecType: "video", Disposition: map[string]int{"attached_pic": 0}}
	subtitle := ffprobeStream{CodecName: "subrip", CodecType: "subtitle"}

	cases := []struct {
		name    string
		format  string
		streams []ffprobeStream
		dur     string
		rejects bool
	}{
		{"plain mp3", "mp3", []ffprobeStream{audioStream("mp3")}, "120.5", false},
		{"mp3 with cover art", "mp3", []ffprobeStream{audioStream("mp3"), attachedPic}, "120.5", false},
		{"flac with cover art", "flac", []ffprobeStream{audioStream("flac"), attachedPic}, "10", false},
		{"ogg vorbis", "ogg", []ffprobeStream{audioStream("vorbis")}, "10", false},
		{"ogg opus", "ogg", []ffprobeStream{audioStream("opus")}, "10", false},
		{"wav pcm_s16le", "wav", []ffprobeStream{audioStream("pcm_s16le")}, "10", false},
		{"wrong codec for container", "mp3", []ffprobeStream{audioStream("flac")}, "10", true},
		{"wav with unlisted pcm variant", "wav", []ffprobeStream{audioStream("adpcm_ms")}, "10", true},
		{"ogg with theora video (not attached pic)", "ogg", []ffprobeStream{audioStream("vorbis"), realVideo}, "10", true},
		{"real video stream only", "mp3", []ffprobeStream{realVideo}, "10", true},
		{"subtitle stream", "mp3", []ffprobeStream{audioStream("mp3"), subtitle}, "10", true},
		{"two audio streams", "mp3", []ffprobeStream{audioStream("mp3"), audioStream("mp3")}, "10", true},
		{"no streams", "mp3", nil, "10", true},
		{"duration exceeds limit", "mp3", []ffprobeStream{audioStream("mp3")}, "99999999", true},
		{"zero duration", "mp3", []ffprobeStream{audioStream("mp3")}, "0", true},
		{"unparseable duration", "mp3", []ffprobeStream{audioStream("mp3")}, "not-a-number", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			probe := ffprobeOutput{Streams: tc.streams, Format: ffprobeFormat{Duration: tc.dur}}
			err := validateAudioStreams(tc.format, probe)
			if tc.rejects && err == nil {
				t.Fatalf("expected rejection, got none")
			}
			if !tc.rejects && err != nil {
				t.Fatalf("expected acceptance, got %v", err)
			}
		})
	}
}

func TestValidateUploadRejectsWrongAudioSignature(t *testing.T) {
	for _, tc := range []struct{ format, ext string }{
		{"wav", ".wav"}, {"flac", ".flac"}, {"ogg", ".ogg"}, {"mp3", ".mp3"},
	} {
		t.Run(tc.format, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "input.bin")
			if err := os.WriteFile(path, []byte("this is not a real audio file at all"), 0600); err != nil {
				t.Fatal(err)
			}
			if _, err := validateUpload(path, "audio"+tc.ext); err == nil {
				t.Fatalf("expected a file with the wrong %s signature to be rejected", tc.format)
			}
		})
	}
}

// TestValidateUploadAcceptsWAVSignatureUpToFFprobe proves the WAV magic-
// byte check itself accepts a real WAV header--distinguishing "rejected
// for a bad signature" from "rejected because ffprobe isn't installed" is
// otherwise impossible on this dev machine, since validateSyntax always
// calls validateAudio last regardless. A wrong-signature case (above)
// fails before ever reaching that point, which this proves by contrast:
// a real WAV header gets past the signature check and only fails at
// validateAudio, with a distinct "audio validation is not available"
// message when ffprobe isn't on PATH, rather than the "signature do not
// match" message a bad header would produce.
func TestValidateUploadAcceptsWAVSignatureUpToFFprobe(t *testing.T) {
	if _, err := exec.LookPath("ffprobe"); err == nil {
		t.Skip("ffprobe is installed; TestConvertAudioEndToEnd covers the full real-tool path instead")
	}
	path := filepath.Join(t.TempDir(), "input.bin")
	if err := os.WriteFile(path, tinyWAV(t, 0.1), 0600); err != nil {
		t.Fatal(err)
	}
	_, err := validateUpload(path, "audio.wav")
	if err == nil {
		t.Fatal("expected validation to fail without ffprobe installed")
	}
	if !strings.Contains(err.Error(), "audio validation is not available") {
		t.Fatalf("expected the missing-ffprobe error, got %v (a real WAV signature should pass the magic-byte check)", err)
	}
}

func TestAudioCapabilitiesDependOnFFmpeg(t *testing.T) {
	c := &converter{}
	if c.supports("wav", "mp3") {
		t.Fatal("audio conversion advertised without ffmpeg/ffprobe")
	}
	c.ffmpeg = filepath.Join("tools", "ffmpeg")
	if c.supports("wav", "mp3") {
		t.Fatal("audio conversion advertised with only ffmpeg, no ffprobe")
	}
	c.ffprobe = filepath.Join("tools", "ffprobe")
	for _, format := range []string{"wav", "flac", "ogg"} {
		if !c.supports(format, "mp3") {
			t.Fatalf("expected %s -> mp3 support with ffmpeg/ffprobe", format)
		}
	}
	if !c.supports("mp3", "wav") {
		t.Fatal("expected mp3 -> wav support with ffmpeg/ffprobe")
	}
	if c.supports("mp3", "mp3") {
		t.Fatal("mp3 -> mp3 must not be advertised")
	}
	if c.supports("mp3", "png") {
		t.Fatal("mp3 -> png must not be advertised")
	}
}

// fakeFFmpegConverter returns a converter whose ffmpeg field points at a
// path that doesn't exist, the same technique fakeLibreOfficeConverter
// uses, so a call site that actually tries to exec it fails distinctly
// rather than succeeding--proving convertAudio was REACHED without
// needing a real ffmpeg install on this machine.
func fakeFFmpegConverter(t *testing.T) *converter {
	t.Helper()
	return &converter{ffmpeg: filepath.Join(t.TempDir(), "not-a-real-ffmpeg"), ffprobe: filepath.Join(t.TempDir(), "not-a-real-ffprobe")}
}

func TestConvertAudioReachesFFmpeg(t *testing.T) {
	c := fakeFFmpegConverter(t)
	inPath := filepath.Join(t.TempDir(), "input.bin")
	if err := os.WriteFile(inPath, tinyWAV(t, 0.1), 0600); err != nil {
		t.Fatal(err)
	}
	outPath := filepath.Join(t.TempDir(), "output.mp3")
	err := c.run(context.Background(), "wav", "mp3", "", inPath, outPath)
	if err == nil {
		t.Fatal("expected an error from the fake ffmpeg binary")
	}
	if !strings.Contains(err.Error(), "audio conversion failed") {
		t.Fatalf("expected the conversion to reach ffmpeg and fail there, got %v", err)
	}
}

// ffmpegLookPath skips the calling test unless both ffmpeg and ffprobe
// are on PATH, mirroring officeLookPath's pattern for LibreOffice-
// dependent tests. Neither is installed on this project's own Windows dev
// machine; these tests run for real in the Docker image and CI.
func ffmpegLookPath(t *testing.T) {
	t.Helper()
	if _, err := exec.LookPath("ffmpeg"); err != nil {
		t.Skip("ffmpeg not installed, skipping audio conversion test")
	}
	if _, err := exec.LookPath("ffprobe"); err != nil {
		t.Skip("ffprobe not installed, skipping audio conversion test")
	}
}

// TestValidateAudioAcceptsRealWAV is the real end-to-end validation case:
// a genuine (if tiny) WAV file must pass validateUpload's full pipeline,
// magic bytes through ffprobe, when ffprobe is actually available.
func TestValidateAudioAcceptsRealWAV(t *testing.T) {
	ffmpegLookPath(t)
	path := filepath.Join(t.TempDir(), "input.bin")
	if err := os.WriteFile(path, tinyWAV(t, 0.2), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := validateUpload(path, "audio.wav"); err != nil {
		t.Fatalf("expected a real WAV fixture to validate, got: %v", err)
	}
}

// TestConvertAudioEndToEnd is the real end-to-end conversion case, skipped
// where ffmpeg/ffprobe aren't installed (this project's own Windows dev
// machine included, same as the LibreOffice- and poppler-dependent
// tests), and running for real in the Docker image and CI.
func TestConvertAudioEndToEnd(t *testing.T) {
	ffmpegLookPath(t)
	c := newConverter()
	inPath := filepath.Join(t.TempDir(), "input.bin")
	if err := os.WriteFile(inPath, tinyWAV(t, 0.5), 0600); err != nil {
		t.Fatal(err)
	}
	for _, out := range []string{"mp3", "flac", "ogg"} {
		t.Run("wav_to_"+out, func(t *testing.T) {
			outPath := filepath.Join(t.TempDir(), "output."+out)
			if err := c.run(context.Background(), "wav", out, "", inPath, outPath); err != nil {
				t.Fatalf("wav -> %s conversion failed: %v", out, err)
			}
			info, err := os.Stat(outPath)
			if err != nil || info.Size() == 0 {
				t.Fatalf("expected a non-empty output file, err=%v", err)
			}
			if err := validateAudio(out, outPath); err != nil {
				t.Fatalf("converted output failed its own audio validation: %v", err)
			}
		})
	}
}

// TestAudioJobFlowsThroughWorker is the full-pipeline regression, mirroring
// TestMarkdownAndHTMLToPDFJobsFlowThroughWorker and TestODFToPDFJobFlowsThroughWorker:
// submit a real WAV job over HTTP and let the actual worker goroutine
// dequeue and convert it exactly as production does. Unlike those,
// this can't be faked with a bogus binary path and still exercise the
// full pipeline--validateAudio needs a real ffprobe at upload time, not
// just at conversion time--so this is skip-guarded rather than using the
// fake-binary technique.
func TestAudioJobFlowsThroughWorker(t *testing.T) {
	ffmpegLookPath(t)
	a := testApp(t)

	w := submitJob(t, a, "clip.wav", tinyWAV(t, 0.3), map[string]string{"outputFormat": "mp3"})
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
		if job.Status != Queued && job.Status != Processing {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if job.Status != Completed {
		t.Fatalf("expected the WAV -> MP3 job to complete, got %s (error: %q)", job.Status, job.Error)
	}
}
