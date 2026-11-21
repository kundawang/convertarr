package helper

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoder/app"
	"encoder/m"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/labstack/gommon/log"
	"gopkg.in/vansante/go-ffprobe.v2"
)

const (
	// TwoPassSafetyMargin reserves a share of the target size for container
	// overhead and audio bitrate estimation errors so the final file stays
	// below the target.
	TwoPassSafetyMargin = 0.95
	// MinVideoBitrateKbps is the lowest video bitrate we consider usable.
	// Below this the target size is rejected as too small.
	MinVideoBitrateKbps = 100
	// DefaultAudioBitrateKbps is the assumed bitrate per retained audio
	// track when ffprobe cannot report one.
	DefaultAudioBitrateKbps = 192
	// TwoPassPrefix is the basename prefix used for passlog files.
	TwoPassPrefix = "convertarr-2pass-"
)

// IsEncoderAvailable checks if a specific ffmpeg encoder is available and if the
// corresponding hardware/drivers are detected on the system.
func IsEncoderAvailable(encoderName string) bool {
	// First, check if the encoder is listed in the ffmpeg build.
	cmd := exec.Command("ffmpeg", "-encoders")
	var out bytes.Buffer
	cmd.Stdout = &out
	err := cmd.Run()
	if err != nil {
		log.Errorf("Failed to run `ffmpeg -encoders`: %v", err)
		return false // Assume not available if ffmpeg command fails
	}
	re, err := regexp.Compile(`\b` + encoderName + `\b`)
	if err != nil {
		log.Errorf("Failed to compile regex for encoder check: %v", err)
		return false
	}
	if !re.MatchString(out.String()) {
		// Encoder not found in the ffmpeg build, so it's definitely not available.
		log.Warnf("Encoder '%s' not found in `ffmpeg -encoders` output.", encoderName)
		return false
	}

	// Next, check for the presence of the required hardware/drivers.
	isNvidia := strings.Contains(encoderName, "nvenc")
	isAmd := strings.Contains(encoderName, "vaapi")

	if isNvidia {
		// Check for NVIDIA hardware by trying to run `nvidia-smi`.
		cmdNv := exec.Command("nvidia-smi")
		if errNv := cmdNv.Run(); errNv != nil {
			log.Warnf("NVIDIA encoder '%s' is in ffmpeg build, but 'nvidia-smi' command failed. Hardware/drivers may be missing or not installed correctly.", encoderName)
			return false
		}
	}

	if isAmd {
		// Check for AMD hardware by looking for a VAAPI render device node.
		files, errAmd := os.ReadDir("/dev/dri")
		if errAmd != nil {
			log.Warnf("Could not read /dev/dri to check for AMD GPU: %v", errAmd)
			return false
		}
		foundRenderNode := false
		for _, file := range files {
			if !file.IsDir() && strings.HasPrefix(file.Name(), "renderD") {
				foundRenderNode = true
				break
			}
		}
		if !foundRenderNode {
			log.Warnf("AMD encoder '%s' is in ffmpeg build, but no /dev/dri/renderD* node was found. Hardware/drivers may be missing or not installed correctly.", encoderName)
			return false
		}
	}

	// If both software and hardware checks pass, the encoder is available.
	return true
}

// UseTwoPassTargetSize reports whether the current settings select the
// software H.264 two-pass target size mode.
func UseTwoPassTargetSize() bool {
	return app.Setting.EnableTargetSizeEncoding &&
		!app.Setting.EnableHevcEncoding &&
		!app.Setting.EnableNvidiaGpuEncoding &&
		!app.Setting.EnableAmdGpuEncoding
}

// CalcTwoPassVideoBitrate budgets the video bitrate (in kbit/s) for a
// two-pass encode from the target file size, the video duration and the
// combined bitrate of the retained (copied) audio tracks.
func CalcTwoPassVideoBitrate(durationSec, audioKbps float64, targetSizeMB int) (float64, error) {
	if durationSec <= 0 {
		return 0, errors.New("video duration is unknown, cannot budget bitrate for target size encoding")
	}
	if targetSizeMB <= 0 {
		return 0, fmt.Errorf("invalid target size %d MB", targetSizeMB)
	}
	targetBits := float64(targetSizeMB) * 1024 * 1024 * 8 * TwoPassSafetyMargin
	totalKbps := targetBits / durationSec / 1000
	videoKbps := totalKbps - audioKbps
	if videoKbps < MinVideoBitrateKbps {
		return 0, fmt.Errorf(
			"target size %d MB is too small for %.1fs of video: total budget %.0f kbit/s, retained audio needs %.0f kbit/s, leaving %.0f kbit/s for video (minimum %d kbit/s)",
			targetSizeMB, durationSec, totalKbps, audioKbps, videoKbps, MinVideoBitrateKbps,
		)
	}
	return videoKbps, nil
}

// RetainedAudioBitrateKbps sums the bitrates (in kbit/s) of all audio
// streams that are copied into the output. Streams without a reported
// bitrate are accounted with DefaultAudioBitrateKbps.
func RetainedAudioBitrateKbps(data *ffprobe.ProbeData) float64 {
	var total float64
	for _, stream := range data.Streams {
		if stream.CodecType != "audio" {
			continue
		}
		kbps := float64(DefaultAudioBitrateKbps)
		if br, err := strconv.ParseFloat(stream.BitRate, 64); err == nil && br > 0 {
			kbps = br / 1000
		}
		total += kbps
	}
	return total
}

// passlogFiles returns all passlog files belonging to a passlog prefix.
func passlogFiles(prefix string) []string {
	files, err := filepath.Glob(prefix + "*")
	if err != nil {
		return nil
	}
	return files
}

// removeStalePasslogs deletes leftover passlog files for the given prefix
// (e.g. from a crashed previous attempt) so a new two-pass encode never
// trips over them. Returns the number of removed files.
func removeStalePasslogs(prefix string) int {
	removed := 0
	for _, f := range passlogFiles(prefix) {
		log.Warnf("Removing stale two-pass passlog file %s", f)
		if err := os.Remove(f); err != nil {
			log.Warnf("Failed to remove stale passlog file %s: %v", f, err)
			continue
		}
		removed++
	}
	return removed
}

// CleanupStalePasslogs removes every Convertarr two-pass passlog left in the
// given directory (used on startup to clean up after a crash or kill).
func CleanupStalePasslogs(dir string) int {
	return removeStalePasslogs(filepath.Join(dir, TwoPassPrefix))
}

// buildTwoPassCommands builds the pass 1 and pass 2 ffmpeg commands for a
// software H.264 target size encode. Stream mapping mirrors the single-pass
// command: first video stream re-encoded, all audio and subtitle streams
// copied with their original mapping.
func buildTwoPassCommands(inputFile, outputFile, passlogPrefix string, videoKbps float64, size, threads int, progressArgPass1, progressArgPass2 string) (string, string) {
	pass1 := fmt.Sprintf(`ffmpeg -i "%s" \
                    -threads %d \
                    -c:v libx264 \
                    -map 0:v:0 \
                    -an -sn \
                    -b:v %.0fk \
                    -pass 1 \
                    -passlogfile "%s" \
                    -pix_fmt yuv420p -profile:v main \
                    -filter:v scale=%d:-2 \
                    -f null /dev/null %s`,
		inputFile,
		threads,
		videoKbps,
		passlogPrefix,
		size,
		progressArgPass1,
	)
	pass2 := fmt.Sprintf(`ffmpeg -i "%s" \
                    -threads %d \
                    -c:v libx264 \
                    -map 0:v:0 \
                    -c:a copy \
                    -c:s copy \
                    -map 0:a? \
                    -map 0:s? \
                    -b:v %.0fk \
                    -pass 2 \
                    -passlogfile "%s" \
                    -pix_fmt yuv420p -profile:v main \
                    -filter:v scale=%d:-2 \
                    "%s" %s`,
		inputFile,
		threads,
		videoKbps,
		passlogPrefix,
		size,
		outputFile,
		progressArgPass2,
	)
	return pass1, pass2
}

// runFfmpeg executes an ffmpeg command string via bash and returns a
// descriptive error (including ffmpeg output) on failure or cancellation.
func runFfmpeg(ctx context.Context, ffmpegCmd string) error {
	cmd := exec.CommandContext(ctx, "bash", "-c", ffmpegCmd)

	var outb, errb bytes.Buffer
	cmd.Stdout = &outb
	cmd.Stderr = &errb

	log.Infof("Executing FFMpeg command: %s", cmd.String())

	if err := cmd.Run(); err != nil {
		if ctx.Err() != nil {
			return fmt.Errorf("encoding cancelled: %v", ctx.Err())
		}
		return fmt.Errorf(
			"error happened while encoding: %v\nout: %v\nerr: %v\nCommand: %s",
			err.Error(), outb.String(), errb.String(), ffmpegCmd,
		)
	}
	return nil
}

// encodeTwoPass runs the software H.264 two-pass target size encode.
func encodeTwoPass(ctx context.Context, inputFile, outputFile string, history *m.History, data *ffprobe.ProbeData, videoDuration float64, allowThreads, size int) error {
	audioKbps := RetainedAudioBitrateKbps(data)
	videoKbps, err := CalcTwoPassVideoBitrate(videoDuration, audioKbps, app.Setting.EncodingTargetSizeMB)
	if err != nil {
		return err
	}
	log.Infof(
		"Two-pass target size: %d MB, duration %.1fs, retained audio %.0f kbit/s, video budget %.0f kbit/s",
		app.Setting.EncodingTargetSizeMB, videoDuration, audioKbps, videoKbps,
	)

	passlogPrefix := filepath.Join(os.TempDir(), TwoPassPrefix+uuid.NewString())

	// A stale passlog from an earlier crashed attempt with the same prefix
	// would corrupt the run; remove it explicitly instead of failing.
	if n := removeStalePasslogs(passlogPrefix); n > 0 {
		log.Warnf("Removed %d stale passlog file(s) for %s before encoding", n, passlogPrefix)
	}
	// Always clean up passlog files (半成品) afterwards, success or failure.
	defer func() {
		if n := removeStalePasslogs(passlogPrefix); n > 0 {
			log.Infof("Cleaned up %d passlog file(s) for %s", n, passlogPrefix)
		}
	}()

	newProgressArg := func(offset, scale float64) string {
		return fmt.Sprintf("-progress unix://%s -y", TempSock(
			videoDuration,
			fmt.Sprintf("%x", sha256.Sum256([]byte(uuid.NewString()))),
			history,
			offset,
			scale,
		))
	}

	pass1Cmd, pass2Cmd := buildTwoPassCommands(
		inputFile, outputFile, passlogPrefix,
		videoKbps, size, allowThreads,
		newProgressArg(0, 0.5),
		newProgressArg(0.5, 0.5),
	)

	// Pass 1: analysis only, no audio/subtitle, output discarded.
	if err := history.EncodingPass(app.DB, 1, 2); err != nil {
		log.Errorf("Failed to update history %v\n", err)
	}
	if err := runFfmpeg(ctx, pass1Cmd); err != nil {
		return fmt.Errorf("two-pass encoding failed during pass 1/2: %w", err)
	}

	if err := ctx.Err(); err != nil {
		return fmt.Errorf("two-pass encoding cancelled after pass 1/2: %v", err)
	}

	// Pass 2: real encode with the original audio/subtitle mapping.
	if err := history.EncodingPass(app.DB, 2, 2); err != nil {
		log.Errorf("Failed to update history %v\n", err)
	}
	if err := runFfmpeg(ctx, pass2Cmd); err != nil {
		// remove the half-finished output file
		if rmErr := os.Remove(outputFile); rmErr != nil && !os.IsNotExist(rmErr) {
			log.Warnf("Failed to remove partial output file %s: %v", outputFile, rmErr)
		}
		return fmt.Errorf("two-pass encoding failed during pass 2/2: %w", err)
	}

	// Verify the result landed within an explainable tolerance of the target.
	if fi, err := os.Stat(outputFile); err == nil {
		targetBytes := float64(app.Setting.EncodingTargetSizeMB) * 1024 * 1024
		newSize := float64(fi.Size())
		if newSize > targetBytes {
			log.Warnf(
				"Two-pass output %.2f MB exceeds target %.2f MB by %.1f%% (container overhead / audio bitrate estimation)",
				newSize/1024/1024, targetBytes/1024/1024, (newSize-targetBytes)/targetBytes*100,
			)
		} else {
			log.Infof("Two-pass output %.2f MB is within target %.2f MB", newSize/1024/1024, targetBytes/1024/1024)
		}
	}

	return nil
}

func Encode(ctx context.Context, inputFile, outputFile string, history *m.History) error {
	timeStart := time.Now()
	timeStartTotal := time.Now()
	// probe file so we can show encoding progress
	// ffprobe context
	probeCtx, cancelFn := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancelFn()

	data, err := ffprobe.ProbeURL(probeCtx, inputFile)
	if err != nil {
		log.Errorf("Failed to probe file %s: %v", inputFile, err)
		return err
	}

	log.Infof("Time Taken => FFProbe: %s", time.Since(timeStart))
	timeStart = time.Now()

	videoDuration := data.Format.Duration().Seconds()

	allowThreads := app.Setting.EncodingThreads
	if allowThreads <= 0 {
		allowThreads = runtime.NumCPU()
	}
	size := app.Setting.EncodingResolution
	crf := app.Setting.EncodingCrf

	// Software H.264 two-pass target size mode
	if UseTwoPassTargetSize() {
		err := encodeTwoPass(ctx, inputFile, outputFile, history, data, videoDuration, allowThreads, size)
		log.Infof("Time Taken => FFMpeg Two-Pass Encode: %s", time.Since(timeStart))
		log.Infof("Time Taken => Total: %s", time.Since(timeStartTotal))
		return err
	}

	// generate ffmpeg command
	var ffmpegCmd string
	progressSocketArg := fmt.Sprintf("-progress unix://%s -y", TempSock(
		videoDuration,
		fmt.Sprintf("%x", sha256.Sum256([]byte(uuid.NewString()))),
		history,
		0,
		1,
	))

	if app.Setting.EnableHevcEncoding {
		// HEVC (H.265) Encoding Logic
		if app.Setting.EnableAmdGpuEncoding {
			log.Info("Encoding HEVC using AMD GPU (VAAPI)")
			ffmpegCmd = fmt.Sprintf(`ffmpeg \
                    -vaapi_device /dev/dri/renderD128 \
                    -i "%s" \
                    -vf 'scale=%d:-2,format=nv12,hwupload' \
                    -c:v hevc_vaapi \
                    -map 0:v:0 \
                    -c:a copy \
                    -c:s copy \
                    -map 0:a? \
                    -map 0:s? \
                    -rc_mode CQP -pix_fmt vaapi -profile:v main \
                    -global_quality %d \
                    "%s" %s`,
				inputFile,
				size,
				crf,
				outputFile,
				progressSocketArg,
			)
		} else if app.Setting.EnableNvidiaGpuEncoding {
			log.Info("Encoding HEVC using NVIDIA GPU (NVENC)")
			ffmpegCmd = fmt.Sprintf(`ffmpeg \
                    -hwaccel cuda -hwaccel_output_format cuda \
                    -i "%s" \
                    -c:v hevc_nvenc \
                    -map 0:v:0 \
                    -c:a copy \
                    -c:s copy \
                    -map 0:a? \
                    -map 0:s? \
                    -rc:v vbr \
                    -cq:v %d \
                    -pix_fmt p010le -profile:v main \
                    -vf "scale=%d:-2" \
                    "%s" %s`,
				inputFile,
				crf,
				size,
				outputFile,
				progressSocketArg,
			)
		} else {
			log.Info("Encoding HEVC using CPU (libx265)")
			ffmpegCmd = fmt.Sprintf(`ffmpeg -i "%s" \
                    -c:v libx265 \
                    -map 0:v:0 \
                    -c:a copy \
                    -c:s copy \
                    -map 0:a? \
                    -map 0:s? \
                    -pix_fmt yuv420p -profile:v main \
                    -x265-params crf=%d:pools=%d -strict experimental \
                    -filter:v scale=%d:-2 \
                    "%s" %s`,
				inputFile,
				crf, allowThreads, size, outputFile,
				progressSocketArg,
			)
		}
	} else {
		// H.264 Encoding Logic
		if app.Setting.EnableAmdGpuEncoding {
			log.Info("Encoding H.264 using AMD GPU (VAAPI)")
			ffmpegCmd = fmt.Sprintf(`ffmpeg \
                    -vaapi_device /dev/dri/renderD128 \
                    -i "%s" \
                    -vf 'scale=%d:-2,format=nv12,hwupload' \
                    -c:v h264_vaapi \
                    -map 0:v:0 \
                    -c:a copy \
                    -c:s copy \
                    -map 0:a? \
                    -map 0:s? \
                    -rc_mode CQP -pix_fmt vaapi \
                    -global_quality %d \
                    "%s" %s`,
				inputFile,
				size,
				crf,
				outputFile,
				progressSocketArg,
			)
		} else if app.Setting.EnableNvidiaGpuEncoding {
			log.Info("Encoding H.264 using NVIDIA GPU (NVENC)")
			ffmpegCmd = fmt.Sprintf(`ffmpeg \
                    -hwaccel cuda -hwaccel_output_format cuda \
                    -i "%s" \
                    -c:v h264_nvenc \
                    -map 0:v:0 \
                    -c:a copy \
                    -c:s copy \
                    -map 0:a? \
                    -map 0:s? \
                    -rc:v vbr \
                    -cq:v %d \
                    -pix_fmt yuv420p -profile:v main \
                    -vf "scale=%d:-2" \
                    "%s" %s`,
				inputFile,
				crf,
				size,
				outputFile,
				progressSocketArg,
			)
		} else {
			log.Info("Encoding H.264 using CPU (libx264)")
			ffmpegCmd = fmt.Sprintf(`ffmpeg -i "%s" \
                    -threads %d \
                    -c:v libx264 \
                    -map 0:v:0 \
                    -c:a copy \
                    -c:s copy \
                    -map 0:a? \
                    -map 0:s? \
                    -pix_fmt yuv420p -profile:v main \
                    -crf %d \
                    -filter:v scale=%d:-2 \
                    "%s" %s`,
				inputFile,
				allowThreads,
				crf, size, outputFile,
				progressSocketArg,
			)
		}
	}

	if err := runFfmpeg(ctx, ffmpegCmd); err != nil {
		return err
	}

	log.Infof("Time Taken => FFMpeg Encode: %s", time.Since(timeStart))
	log.Infof("Time Taken => Total: %s", time.Since(timeStartTotal))

	return nil
}
