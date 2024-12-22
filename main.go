package main

import (
	"bytes"
	"encoding/binary"
	"encoding/json"
	"flag"
	"fmt"
	"image"
	"image/color"
	"image/png"
	"log"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"
)

type MediaInfo struct {
	Streams []struct {
		CodecType  string `json:"codec_type"`
		Channels   int    `json:"channels"`
		SampleRate string `json:"sample_rate"`
	} `json:"streams"`
}

type AudioFormat struct {
	Format struct {
		FormatName string `json:"format_name"`
		Duration   string `json:"duration"`
	} `json:"format"`
	Streams []struct {
		CodecType string `json:"codec_type"`
		CodecName string `json:"codec_name"`
		Duration  string `json:"duration"`
		Channels  int    `json:"channels"`
		BitRate   string `json:"bit_rate,omitempty"`
	} `json:"streams"`
}

func fatal(msg string) {
	log.Fatal("error: " + msg)
}

func checkFFmpegInstallation() {
	// Check ffmpeg installation
	cmd := exec.Command("ffmpeg", "-version")
	err := cmd.Run()
	if err != nil {
		fatal("FFmpeg is not installed or not accessible in your PATH.")
	}

	// Check ffprobe installation
	cmd = exec.Command("ffprobe", "-version")
	err = cmd.Run()
	if err != nil {
		fatal("FFprobe is not installed or not accessible in your PATH.")
	}

	fmt.Printf("FFmpeg and FFprobe are installed and accessible.\n\n")
}

func checkAudioFile(path string) string {
	if _, err := os.Stat(path); os.IsNotExist(err) {
		fatal(fmt.Sprintf("Audio file '%s' does not exist", path))
	}
	return path
}

func runCommand(command []string, decodeOutput bool) []byte {
	cmd := exec.Command(command[0], command[1:]...)
	output, err := cmd.Output()
	if err != nil {
		fatal(fmt.Sprintf("Command '%s' failed: %v", strings.Join(command, " "), err))
	}
	return output
}

func readInfo(media string) MediaInfo {
	command := []string{
		"ffprobe",
		"-loglevel", "panic",
		media,
		"-print_format", "json",
		"-show_format",
		"-show_streams",
	}
	result := runCommand(command, true)

	var info MediaInfo
	if err := json.Unmarshal(result, &info); err != nil {
		fatal(fmt.Sprintf("Failed to parse media info: %v", err))
	}
	return info
}

func verifyAudioFormat(filepath string) error {
	cmd := exec.Command("ffmpeg", "-i", filepath)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	cmd.Run() // We expect this to "fail" as it's just a probe

	// Check if ffmpeg is installed
	if stderr.Len() == 0 {
		return fmt.Errorf("FFmpeg is not installed or not accessible in PATH")
	}

	// Get detailed format information
	cmd = exec.Command("ffprobe",
		"-v", "quiet",
		"-print_format", "json",
		"-show_format",
		"-show_streams",
		filepath)

	var stdout bytes.Buffer
	cmd.Stdout = &stdout
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("failed to probe audio file: %v", err)
	}

	var format AudioFormat
	if err := json.Unmarshal(stdout.Bytes(), &format); err != nil {
		return fmt.Errorf("failed to parse audio format: %v", err)
	}

	// Verify it has audio streams
	hasAudio := false
	for _, stream := range format.Streams {
		if stream.CodecType == "audio" {
			hasAudio = true
			break
		}
	}
	if !hasAudio {
		return fmt.Errorf("no audio streams found in file")
	}

	// Print audio information
	fmt.Println("\nAudio File Information:")
	fmt.Printf("Format: %s\n", format.Format.FormatName)

	for _, stream := range format.Streams {
		if stream.CodecType == "audio" {
			fmt.Printf("Codec: %s\n", stream.CodecName)
			if stream.BitRate != "" {
				bitrate, _ := strconv.Atoi(stream.BitRate)
				fmt.Printf("Bitrate: %d kbps\n", bitrate/1000)
			}
			fmt.Printf("Channels: %d\n", stream.Channels)
			if duration, err := strconv.ParseFloat(stream.Duration, 64); err == nil {
				fmt.Printf("Duration: %.2f seconds\n", duration)
			}
		}
	}
	fmt.Println()

	return nil
}

func readAudio(audio string, seek *float64, duration *float64) ([][]float32, float64) {
	info := readInfo(audio)
	if len(info.Streams) == 0 || info.Streams[0].CodecType != "audio" {
		fatal(fmt.Sprintf("%s should contain only audio", audio))
	}

	channels := info.Streams[0].Channels
	sampleRate, _ := strconv.ParseFloat(info.Streams[0].SampleRate, 64)

	command := []string{"ffmpeg", "-y", "-loglevel", "panic"}
	if seek != nil {
		command = append(command, "-ss", fmt.Sprintf("%f", *seek))
	}
	command = append(command, "-i", audio)
	if duration != nil {
		command = append(command, "-t", fmt.Sprintf("%f", *duration))
	}
	command = append(command, "-f", "f32le", "-")

	rawAudio := runCommand(command, false)
	samples := len(rawAudio) / 4 / channels
	wav := make([][]float32, channels)
	for i := range wav {
		wav[i] = make([]float32, samples)
	}

	for i := 0; i < samples; i++ {
		for c := 0; c < channels; c++ {
			offset := 4 * (i*channels + c)
			bits := binary.LittleEndian.Uint32(rawAudio[offset : offset+4])
			wav[c][i] = math.Float32frombits(bits)
		}
	}

	return wav, sampleRate
}

func sigmoid(x float64) float64 {
	return 1.0 / (1.0 + math.Exp(-x))
}

func envelope(wav []float32, window, stride int) []float64 {
	padded := make([]float32, len(wav)+window)
	copy(padded[window/2:], wav)

	var out []float64
	for off := 0; off < len(padded)-window; off += stride {
		frame := padded[off : off+window]
		sum := 0.0
		count := 0
		for _, v := range frame {
			if v > 0 {
				sum += float64(v)
				count++
			}
		}
		if count > 0 {
			out = append(out, sum/float64(count))
		} else {
			out = append(out, 0)
		}
	}

	for i := range out {
		out[i] = 1.9 * (sigmoid(2.5*out[i]) - 0.5)
	}
	return out
}

func drawEnv(env []float64, outPath string, fgColor, bgColor color.Color, width, height int) {
	img := image.NewRGBA(image.Rect(0, 0, width, height))

	// Fill background
	for y := 0; y < height; y++ {
		for x := 0; x < width; x++ {
			img.Set(x, y, bgColor)
		}
	}

	barWidth := float64(width) / float64(len(env))
	padRatio := 0.1
	actualWidth := barWidth / (1 + 2*padRatio)
	pad := padRatio * actualWidth

	for i, e := range env {
		half := 0.5 * e
		x := int(pad + float64(i)*barWidth)
		barW := int(actualWidth)
		mid := height / 2

		// Draw upper half
		for y := mid - int(half*float64(height)/2); y < mid; y++ {
			for dx := 0; dx < barW; dx++ {
				img.Set(x+dx, y, fgColor)
			}
		}

		// Draw lower half with transparency
		lowerFg := color.RGBA{
			R: fgColor.(color.RGBA).R,
			G: fgColor.(color.RGBA).G,
			B: fgColor.(color.RGBA).B,
			A: 204, // ~0.8 opacity
		}
		for y := mid; y < mid+int(0.9*half*float64(height)/2); y++ {
			for dx := 0; dx < barW; dx++ {
				img.Set(x+dx, y, lowerFg)
			}
		}
	}

	f, err := os.Create(outPath)
	if err != nil {
		fatal(fmt.Sprintf("Failed to create output file: %v", err))
	}
	defer f.Close()
	png.Encode(f, img)
}

func interpole(x1, y1, x2, y2, x float64) float64 {
	return y1 + (y2-y1)*(x-x1)/(x2-x1)
}

func parseColor(colorStr string) color.Color {
	if strings.HasPrefix(colorStr, "#") {
		re := regexp.MustCompile(`^#(?:[0-9a-fA-F]{3}){1,2}$`)
		if !re.MatchString(colorStr) {
			fatal("Color must be a valid hexadecimal color code")
		}
		colorStr = strings.TrimPrefix(colorStr, "#")
		if len(colorStr) == 3 {
			colorStr = string([]byte{colorStr[0], colorStr[0], colorStr[1], colorStr[1], colorStr[2], colorStr[2]})
		}
		r, _ := strconv.ParseUint(colorStr[0:2], 16, 8)
		g, _ := strconv.ParseUint(colorStr[2:4], 16, 8)
		b, _ := strconv.ParseUint(colorStr[4:6], 16, 8)
		return color.RGBA{uint8(r), uint8(g), uint8(b), 255}
	}

	// Handle RGB format
	parts := strings.Split(colorStr, ",")
	if len(parts) != 3 {
		fatal("Color must be in format 0.xx,0.xx,0.xx or #RRGGBB")
	}
	r, _ := strconv.ParseFloat(parts[0], 64)
	g, _ := strconv.ParseFloat(parts[1], 64)
	b, _ := strconv.ParseFloat(parts[2], 64)
	return color.RGBA{uint8(r * 255), uint8(g * 255), uint8(b * 255), 255}
}

func startLoader(stopChan chan bool, customText string) {
	loaderChars := []string{"⠋", "⠙", "⠹", "⠸", "⠼", "⠴", "⠦", "⠧", "⠇", "⠏"}
	index := 0
	for {
		select {
		case <-stopChan:
			fmt.Printf("\r%-80s\r", "")
			return
		default:
			fmt.Printf("\r%s %s", loaderChars[index], customText)
			index = (index + 1) % len(loaderChars)
			time.Sleep(80 * time.Millisecond)
		}
	}
}

func visualize(audio string, tmpDir string, format string, out string, seek, duration *float64, rate, bars int,
	speed, time, oversample float64, fgColor, bgColor color.Color, width, height int) {

	// Read the audio and its sample rate
	wav, sr := readAudio(audio, seek, duration)

	// Convert to mono
	mono := make([]float32, len(wav[0]))
	for i := range mono {
		sum := float32(0)
		for c := range wav {
			sum += wav[c][i]
		}
		mono[i] = sum / float32(len(wav))
	}

	// Normalize the audio
	var std float32
	for _, v := range mono {
		std += v * v
	}
	std = float32(math.Sqrt(float64(std / float32(len(mono)))))
	for i := range mono {
		mono[i] /= std
	}

	window := int(sr * time / float64(bars))
	stride := int(float64(window) / oversample)
	env := envelope(mono, window, stride)

	audioDuration := float64(len(mono)) / sr
	frames := int(float64(rate) * audioDuration)

	// Pad the envelope to ensure smooth transitions
	padded := make([]float64, len(env)+3*bars)
	copy(padded[bars/2:], env)
	env = padded

	// Start the loading animation
	stopChan := make(chan bool)
	go startLoader(stopChan, "Generating frames...")

	// Create frames in parallel using a goroutine pool
	frameChan := make(chan int, frames)
	var wg sync.WaitGroup
	for idx := 0; idx < frames; idx++ {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			pos := (float64(idx) / float64(rate) * sr) / float64(stride) / float64(bars)
			off := int(pos)
			loc := pos - float64(off)

			env1 := env[off*bars : (off+1)*bars]
			env2 := env[(off+1)*bars : (off+2)*bars]

			var maxvol float64
			for _, v := range env2 {
				if v > maxvol {
					maxvol = v
				}
			}
			maxvol = math.Log10(1e-4+maxvol) * 10
			speedup := math.Max(0.5, math.Min(2, interpole(-6, 0.5, 0, 2, maxvol)))
			w := sigmoid(speed * speedup * (loc - 0.5))

			denv := make([]float64, bars)
			for i := range denv {
				denv[i] = (1-w)*env1[i] + w*env2[i]
				denv[i] *= 0.5 * (1 - math.Cos(2*math.Pi*float64(i)/float64(len(denv)-1)))
			}

			outPath := filepath.Join(tmpDir, fmt.Sprintf("%06d.png", idx))
			drawEnv(denv, outPath, fgColor, bgColor, width, height)
			frameChan <- idx
		}(idx)
	}

	// Wait for all frames to be generated
	wg.Wait()

	// Stop the loading animation for frames
	stopChan <- true

	// Start encoding video
	go startLoader(stopChan, "Encoding video with FFmpeg...")

	// Prepare the base FFmpeg command
	ffmpegCmd := []string{
		"ffmpeg", "-y",
		"-framerate", fmt.Sprintf("%d", rate),
		"-i", filepath.Join(tmpDir, "%06d.png"),
	}

	// Handle seek and duration options
	if seek != nil {
		ffmpegCmd = append(ffmpegCmd, "-ss", fmt.Sprintf("%f", *seek))
	}
	ffmpegCmd = append(ffmpegCmd, "-i", audio)
	if duration != nil {
		ffmpegCmd = append(ffmpegCmd, "-t", fmt.Sprintf("%f", *duration))
	}

	// Handle format-specific options
	if strings.ToLower(format) == "webm" {
		// WebM-specific FFmpeg command (VP8 for video, Opus for audio)
		ffmpegCmd = append(ffmpegCmd,
			"-c:v", "libvpx",
			"-c:a", "libopus",
			"-deadline", "best",
			"-pix_fmt", "yuv420p",
			"-shortest",
			"-f", "webm",
		)
	} else if strings.ToLower(format) == "mkv" {
		ffmpegCmd = append(ffmpegCmd,
			"-c:v", "libx264",
			"-c:a", "aac",
			"-pix_fmt", "yuv420p",
			"-shortest",
			"-f", "matroska",
		)
	} else {
		ffmpegCmd = append(ffmpegCmd,
			"-c:v", "libx264",
			"-c:a", "aac",
			"-pix_fmt", "yuv420p",
			"-shortest",
			"-movflags", "+faststart",
			"-f", format,
		)
	}

	// Output file path
	ffmpegCmd = append(ffmpegCmd, out)

	// Run the FFmpeg command
	cmd := exec.Command(ffmpegCmd[0], ffmpegCmd[1:]...)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		stopChan <- true
		fatal(fmt.Sprintf("Failed to encode video: %v\n%s", err, stderr.String()))
	}

	// Stop the loading animation
	stopChan <- true

	// Clean up temporary PNG files
	matches, _ := filepath.Glob(filepath.Join(tmpDir, "*.png"))
	for _, f := range matches {
		os.Remove(f)
	}

	fmt.Printf("Audio wave video generation completed: %s\n", out)
}

func showBanner() {
	banner := `

 ██████╗  ██████╗  █████╗ ██████╗ ██╗    ██╗
██╔════╝ ██╔═══██╗██╔══██╗╚════██╗██║    ██║
██║  ███╗██║   ██║███████║ █████╔╝██║ █╗ ██║
██║   ██║██║   ██║██╔══██║██╔═══╝ ██║███╗██║
╚██████╔╝╚██████╔╝██║  ██║███████╗╚███╔███╔╝
 ╚═════╝  ╚═════╝ ╚═╝  ╚═╝╚══════╝ ╚══╝╚══╝ 

Go Audio to Waveform Video Generator
`
	fmt.Println(banner)
}

// Helper function to format duration as a readable string
func formatDuration(d time.Duration) string {
	seconds := int(d.Seconds())
	hours := seconds / 3600
	minutes := (seconds % 3600) / 60
	seconds = seconds % 60

	// Build the formatted string, including only relevant units
	var formatted string
	if hours > 0 {
		formatted += fmt.Sprintf("%d hours ", hours)
	}
	if minutes > 0 || hours > 0 {
		formatted += fmt.Sprintf("%d minutes ", minutes)
	}
	formatted += fmt.Sprintf("%d seconds", seconds)

	return formatted
}

// Helper function to print the current flag settings
func printSettings(rate int, fgColorStr, bgColorStr string, bars int, oversample, timeVal, speed float64,
	width, height int, seek, duration float64, inputFile, format string) {

	fmt.Printf("--- Current Settings ---\n")
	fmt.Printf("Input Audio File: %s\n", inputFile)
	fmt.Printf("Output Video Format: %s\n", format)
	fmt.Printf("Video Framerate: %d fps\n", rate)
	fmt.Printf("Number of Bars: %d\n", bars)
	fmt.Printf("Audio Duration per Frame: %.2f seconds\n", timeVal)
	fmt.Printf("Speed Factor: %.2f\n", speed)
	fmt.Printf("Audio Oversampling: %.2f\n", oversample)
	fmt.Printf("Width: %d px\n", width)
	fmt.Printf("Height: %d px\n", height)

	if seek > 0 {
		fmt.Printf("Seek Start Time: %.2f seconds\n", seek)
	}
	if duration > 0 {
		fmt.Printf("Duration: %.2f seconds\n", duration)
	}

	fmt.Printf("Foreground Color: %s\n", fgColorStr)
	fmt.Printf("Background Color: %s\n", bgColorStr)
	fmt.Printf("-------------------------\n\n")
}

func main() {
	showBanner()

	// Check if FFmpeg is installed
	checkFFmpegInstallation()

	// Flag definitions
	rate := flag.Int("r", 60, "Video framerate")
	fgColorStr := flag.String("fg", "#007D9C", "Foreground color (RGB or hex: #RRGGBB)")
	bgColorStr := flag.String("bg", "#000000", "Background color (RGB or hex: #RRGGBB)")
	bars := flag.Int("b", 50, "Number of bars on the video at once")
	oversample := flag.Float64("oversample", 5.0, "Lower values will feel less reactive")
	timeVal := flag.Float64("t", 0.40, "Amount of audio shown at once on a frame")
	speed := flag.Float64("speed", 3.5, "Higher values mean faster transitions between frames")
	width := flag.Int("w", 800, "Width in pixels of the animation")
	height := flag.Int("h", 600, "Height in pixels of the animation")
	seek := flag.Float64("seek", 0.0, "Seek to time in seconds in the video")
	duration := flag.Float64("duration", 0.0, "Duration in seconds from the seek time")
	inputFile := flag.String("i", "", "Input audio file")
	format := flag.String("format", "mp4", "Output video format (e.g., mp4, avi, mkv, webm, mov)")

	// Custom usage message
	flag.Usage = func() {
		fmt.Fprintf(os.Stderr, "Usage: %s [options] \n\nOptions:\n", os.Args[0])
		flag.PrintDefaults()
	}

	// Parse flags
	flag.Parse()

	// Check if audio file is provided
	if *inputFile == "" {
		fmt.Println("Error: You must specify an input file using the -i flag.")
		flag.Usage()
		os.Exit(1)
	}

	audioFile := *inputFile
	checkAudioFile(audioFile)

	// Start loader for format verification
	stopChan := make(chan bool)
	go startLoader(stopChan, "Verifying audio format")

	// Verify audio format
	if err := verifyAudioFormat(audioFile); err != nil {
		stopChan <- true
		fatal(fmt.Sprintf("Audio format verification failed: %v", err))
	}
	stopChan <- true

	// Print the current settings being used for the generation
	printSettings(*rate, *fgColorStr, *bgColorStr, *bars, *oversample, *timeVal, *speed,
		*width, *height, *seek, *duration, *inputFile, *format)

	// Create temporary directory
	tmpDir, err := os.MkdirTemp("", "seewav_*")
	if err != nil {
		fatal(fmt.Sprintf("Failed to create temporary directory: %v", err))
	}
	defer os.RemoveAll(tmpDir)

	// Parse colors
	fgColor := parseColor(*fgColorStr)
	bgColor := parseColor(*bgColorStr)

	// Handle seek and duration
	var seekPtr, durationPtr *float64
	if *seek > 0 {
		seekPtr = seek
	}
	if *duration > 0 {
		durationPtr = duration
	}

	// Extract filename without extension
	baseName := filepath.Base(audioFile)
	ext := filepath.Ext(baseName)
	nameWithoutExt := strings.TrimSuffix(baseName, ext)
	re := regexp.MustCompile(`\W+| |-|\.`)
	safeName := re.ReplaceAllString(nameWithoutExt, "_")
	outFile := safeName + strings.ReplaceAll(ext, ".", "_") + "." + *format

	// Start timing the generation process
	startTime := time.Now()

	// Call the visualize function with all the arguments
	visualize(audioFile, tmpDir, *format, outFile, seekPtr, durationPtr, *rate, *bars,
		*speed, *timeVal, *oversample, fgColor, bgColor, *width, *height)

	// Calculate the duration
	elapsedTime := time.Since(startTime)

	// Format the elapsed time using the helper function
	formattedTime := formatDuration(elapsedTime)

	// Print the generation completion message
	fmt.Printf("Generation completed in: %s\n", formattedTime)
}
