package helper

import (
	"context"
	"encoder/app"
	"encoder/m"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"gopkg.in/vansante/go-ffprobe.v2"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
)

// setupTestEnv gives every test an isolated in-memory database and a fresh
// settings object.
func setupTestEnv(t *testing.T) {
	t.Helper()
	dsn := fmt.Sprintf("file:%s?mode=memory&cache=shared",
		strings.NewReplacer("/", "_", " ", "_").Replace(t.Name()))
	db, err := gorm.Open(sqlite.Open(dsn), &gorm.Config{})
	if err != nil {
		t.Fatalf("failed to open test db: %v", err)
	}
	if err := db.AutoMigrate(&m.History{}); err != nil {
		t.Fatalf("failed to migrate test db: %v", err)
	}
	app.DB = db
	app.Setting = &m.SettingValue{
		EnableEncoding:           true,
		EnableTargetSizeEncoding: true,
		EncodingTargetSizeMB:     1,
		EncodingThreads:          2,
		EncodingCrf:              25,
		EncodingResolution:       640,
	}
}

func requireFfmpeg(t *testing.T) {
	t.Helper()
	for _, bin := range []string{"ffmpeg", "ffprobe"} {
		if _, err := exec.LookPath(bin); err != nil {
			t.Skipf("%s not available, skipping integration test", bin)
		}
	}
}

// createVideoFixture renders a short mkv with a video, an audio and a
// subtitle stream so stream mapping can be verified after encoding.
func createVideoFixture(t *testing.T, dir string) string {
	t.Helper()
	requireFfmpeg(t)
	srtPath := filepath.Join(dir, "fixture.srt")
	srt := "1\n00:00:00,000 --> 00:00:05,000\nhello world\n"
	if err := os.WriteFile(srtPath, []byte(srt), 0o600); err != nil {
		t.Fatalf("failed to write srt: %v", err)
	}
	out := filepath.Join(dir, "input.mkv")
	cmd := exec.Command("ffmpeg", "-y",
		"-f", "lavfi", "-i", "testsrc=duration=6:rate=25:size=640x360,noise=alls=25:allf=t+u",
		"-f", "lavfi", "-i", "sine=frequency=440:duration=6",
		"-f", "srt", "-i", srtPath,
		"-c:v", "libx264", "-crf", "20", "-pix_fmt", "yuv420p",
		"-c:a", "aac", "-b:a", "128k",
		"-c:s", "srt",
		"-shortest",
		out,
	)
	if o, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("failed to create video fixture: %v\n%s", err, o)
	}
	return out
}

// createAudioOnlyFixture renders a short mkv without any video stream.
func createAudioOnlyFixture(t *testing.T, dir string) string {
	t.Helper()
	requireFfmpeg(t)
	out := filepath.Join(dir, "audioonly.mkv")
	cmd := exec.Command("ffmpeg", "-y",
		"-f", "lavfi", "-i", "sine=frequency=440:duration=3",
		"-c:a", "aac", "-b:a", "128k",
		out,
	)
	if o, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("failed to create audio fixture: %v\n%s", err, o)
	}
	return out
}

func newHistory(t *testing.T, oldPath string) *m.History {
	t.Helper()
	h := &m.History{}
	if err := h.Create(app.DB, oldPath); err != nil {
		t.Fatalf("failed to create history: %v", err)
	}
	return h
}

func passlogLeftovers(t *testing.T) []string {
	t.Helper()
	files, err := filepath.Glob(filepath.Join(os.TempDir(), TwoPassPrefix+"*"))
	if err != nil {
		t.Fatalf("failed to glob passlogs: %v", err)
	}
	return files
}

func TestCalcTwoPassVideoBitrate(t *testing.T) {
	// 50 MB, 600 s, two 128 kbit/s audio tracks
	kbps, err := CalcTwoPassVideoBitrate(600, 256, 50)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	// total budget: 50*1024*1024*8*0.95/600/1000 = 664.1 kbit/s
	want := 664.1 - 256
	if kbps < want-1 || kbps > want+1 {
		t.Fatalf("unexpected video bitrate %.2f, want ~%.2f", kbps, want)
	}

	// unknown duration
	if _, err := CalcTwoPassVideoBitrate(0, 128, 50); err == nil ||
		!strings.Contains(err.Error(), "duration is unknown") {
		t.Fatalf("expected unknown duration error, got %v", err)
	}

	// budget too small: 1 MB over 600 s leaves nothing for video
	if _, err := CalcTwoPassVideoBitrate(600, 128, 1); err == nil ||
		!strings.Contains(err.Error(), "too small") {
		t.Fatalf("expected insufficient budget error, got %v", err)
	}

	// invalid target size
	if _, err := CalcTwoPassVideoBitrate(600, 128, 0); err == nil {
		t.Fatal("expected invalid target size error")
	}
}

func TestRetainedAudioBitrateKbps(t *testing.T) {
	data := &ffprobe.ProbeData{
		Streams: []*ffprobe.Stream{
			{CodecType: "video", BitRate: "5000000"},
			{CodecType: "audio", BitRate: "128000"},
			{CodecType: "audio", BitRate: ""}, // unknown -> default
			{CodecType: "subtitle"},
		},
	}
	got := RetainedAudioBitrateKbps(data)
	want := float64(128) + float64(DefaultAudioBitrateKbps)
	if got != want {
		t.Fatalf("unexpected audio budget %.0f, want %.0f", got, want)
	}
}

func TestBuildTwoPassCommands(t *testing.T) {
	pass1, pass2 := buildTwoPassCommands(
		"/in.mkv", "/out.mkv", "/tmp/convertarr-2pass-test",
		1200, 640, 4, "-progress unix:///tmp/s1 -y", "-progress unix:///tmp/s2 -y",
	)

	for _, want := range []string{
		"-c:v libx264", "-b:v 1200k", "-pass 1",
		`-passlogfile "/tmp/convertarr-2pass-test"`,
		"-map 0:v:0", "-an -sn", "-f null /dev/null",
		"-threads 4", "scale=640:-2",
	} {
		if !strings.Contains(pass1, want) {
			t.Errorf("pass 1 command missing %q:\n%s", want, pass1)
		}
	}
	// pass 1 must not keep audio/subtitles
	if strings.Contains(pass1, "-c:a copy") || strings.Contains(pass1, "0:a?") {
		t.Errorf("pass 1 command must not map audio/subtitles:\n%s", pass1)
	}

	for _, want := range []string{
		"-c:v libx264", "-b:v 1200k", "-pass 2",
		`-passlogfile "/tmp/convertarr-2pass-test"`,
		"-map 0:v:0", "-map 0:a?", "-map 0:s?", "-c:a copy", "-c:s copy",
		`"/out.mkv"`,
	} {
		if !strings.Contains(pass2, want) {
			t.Errorf("pass 2 command missing %q:\n%s", want, pass2)
		}
	}
}

func TestEncodeTwoPassTargetSize(t *testing.T) {
	setupTestEnv(t)
	dir := t.TempDir()
	input := createVideoFixture(t, dir)
	output := filepath.Join(dir, "output.mkv")

	history := newHistory(t, input)
	if err := Encode(context.Background(), input, output, history); err != nil {
		t.Fatalf("two-pass encode failed: %v", err)
	}

	// output size within explainable tolerance of the 1 MB target
	fi, err := os.Stat(output)
	if err != nil {
		t.Fatalf("output missing: %v", err)
	}
	const target = 1 * 1024 * 1024
	if fi.Size() > target {
		t.Fatalf("output %d bytes exceeds target %d bytes", fi.Size(), target)
	}
	if fi.Size() < target*6/10 {
		t.Fatalf("output %d bytes suspiciously small (< 60%% of target)", fi.Size())
	}

	// stream mapping preserved: h264 video, copied audio, copied subtitle
	probeCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	data, err := ffprobe.ProbeURL(probeCtx, output)
	if err != nil {
		t.Fatalf("failed to probe output: %v", err)
	}
	var video, audio, subtitle int
	for _, s := range data.Streams {
		switch s.CodecType {
		case "video":
			video++
			if s.CodecName != "h264" {
				t.Fatalf("expected h264 video, got %s", s.CodecName)
			}
		case "audio":
			audio++
			if s.CodecName != "aac" {
				t.Fatalf("expected copied aac audio, got %s", s.CodecName)
			}
		case "subtitle":
			subtitle++
		}
	}
	if video != 1 || audio != 1 || subtitle != 1 {
		t.Fatalf("unexpected stream mapping: video=%d audio=%d subtitle=%d", video, audio, subtitle)
	}

	// history reflects the two-pass state
	if history.Status != "encoding pass 2/2" {
		t.Fatalf("unexpected history status %q", history.Status)
	}
	// progress socket reports asynchronously; wait for the final value
	deadline := time.Now().Add(3 * time.Second)
	for history.Progress < 0.99 && time.Now().Before(deadline) {
		time.Sleep(50 * time.Millisecond)
	}
	if history.Progress < 0.99 {
		t.Fatalf("progress did not reach ~1.0, got %.2f", history.Progress)
	}

	// passlog files cleaned up
	if left := passlogLeftovers(t); len(left) > 0 {
		t.Fatalf("passlog files left behind: %v", left)
	}
}

func TestEncodeTwoPassUnknownDuration(t *testing.T) {
	setupTestEnv(t)
	history := newHistory(t, "x")
	err := encodeTwoPass(context.Background(), "in", "out", history, &ffprobe.ProbeData{}, 0, 2, 640)
	if err == nil || !strings.Contains(err.Error(), "duration is unknown") {
		t.Fatalf("expected unknown duration error, got %v", err)
	}
}

func TestEncodeTwoPassInsufficientBudget(t *testing.T) {
	setupTestEnv(t)
	app.Setting.EncodingTargetSizeMB = 1
	history := newHistory(t, "x")
	// 1 MB for one hour of video with audio is impossible
	err := encodeTwoPass(context.Background(), "in", "out", history, &ffprobe.ProbeData{
		Streams: []*ffprobe.Stream{{CodecType: "audio", BitRate: "128000"}},
	}, 3600, 2, 640)
	if err == nil || !strings.Contains(err.Error(), "too small") {
		t.Fatalf("expected insufficient budget error, got %v", err)
	}
}

func TestEncodeTwoPassFirstPassFailure(t *testing.T) {
	setupTestEnv(t)
	dir := t.TempDir()
	input := createAudioOnlyFixture(t, dir) // no video stream -> pass 1 fails
	output := filepath.Join(dir, "output.mkv")

	history := newHistory(t, input)
	err := Encode(context.Background(), input, output, history)
	if err == nil || !strings.Contains(err.Error(), "pass 1/2") {
		t.Fatalf("expected pass 1 failure, got %v", err)
	}
	if _, statErr := os.Stat(output); !os.IsNotExist(statErr) {
		t.Fatalf("partial output should not exist after pass 1 failure")
	}
	if left := passlogLeftovers(t); len(left) > 0 {
		t.Fatalf("passlog files left behind: %v", left)
	}
}

func TestEncodeTwoPassCancel(t *testing.T) {
	setupTestEnv(t)
	dir := t.TempDir()
	input := createVideoFixture(t, dir)
	output := filepath.Join(dir, "output.mkv")

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // cancel before the encode starts

	history := newHistory(t, input)
	err := Encode(ctx, input, output, history)
	if err == nil || !strings.Contains(err.Error(), "cancel") {
		t.Fatalf("expected cancellation error, got %v", err)
	}
	if _, statErr := os.Stat(output); !os.IsNotExist(statErr) {
		t.Fatalf("partial output should not exist after cancellation")
	}
	if left := passlogLeftovers(t); len(left) > 0 {
		t.Fatalf("passlog files left behind: %v", left)
	}
}

func TestStalePasslogCleanup(t *testing.T) {
	dir := t.TempDir()
	prefix := filepath.Join(dir, TwoPassPrefix+"abc")
	for _, suffix := range []string{"-0.log", "-0.log.mbtree"} {
		if err := os.WriteFile(prefix+suffix, []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if n := removeStalePasslogs(prefix); n != 2 {
		t.Fatalf("expected 2 removed passlogs, got %d", n)
	}
	if left, _ := filepath.Glob(prefix + "*"); len(left) != 0 {
		t.Fatalf("passlogs not cleaned: %v", left)
	}

	// startup sweep only touches convertarr passlogs
	other := filepath.Join(dir, "other-0.log")
	if err := os.WriteFile(other, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	stale := filepath.Join(dir, TwoPassPrefix+"old-0.log")
	if err := os.WriteFile(stale, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if n := CleanupStalePasslogs(dir); n != 1 {
		t.Fatalf("expected 1 removed stale passlog, got %d", n)
	}
	if _, err := os.Stat(other); err != nil {
		t.Fatalf("unrelated file must not be removed: %v", err)
	}
}

// TestEncodeSinglePassCrfUnchanged makes sure the original CRF path still
// works when target size mode is disabled.
func TestEncodeSinglePassCrfUnchanged(t *testing.T) {
	setupTestEnv(t)
	app.Setting.EnableTargetSizeEncoding = false
	dir := t.TempDir()
	input := createVideoFixture(t, dir)
	output := filepath.Join(dir, "output.mkv")

	history := newHistory(t, input)
	if err := Encode(context.Background(), input, output, history); err != nil {
		t.Fatalf("single-pass encode failed: %v", err)
	}
	if _, err := os.Stat(output); err != nil {
		t.Fatalf("output missing: %v", err)
	}
	if history.Status == "encoding pass 1/2" || history.Status == "encoding pass 2/2" {
		t.Fatalf("single-pass encode must not use two-pass statuses, got %q", history.Status)
	}
}
