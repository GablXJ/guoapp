package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
)

type probeStream struct {
	CodecType         string `json:"codec_type"`
	CodecName         string `json:"codec_name"`
	Profile           string `json:"profile"`
	PixFmt            string `json:"pix_fmt"`
	SampleAspectRatio string `json:"sample_aspect_ratio"`
	ColorTransfer     string `json:"color_transfer"`
	ColorPrimaries    string `json:"color_primaries"`
	ChannelLayout     string `json:"channel_layout"`
	ExtradataHash     string `json:"extradata_hash"`
	SampleRate        string `json:"sample_rate"`
	Width             int    `json:"width"`
	Height            int    `json:"height"`
	Channels          int    `json:"channels"`
}

type probeFormat struct {
	Duration string `json:"duration"`
}

type mediaProbe struct {
	Streams []probeStream  `json:"streams"`
	Format  probeFormat    `json:"format"`
	Raw     map[string]any `json:"-"`
}

func (probe mediaProbe) video() *probeStream {
	for index := range probe.Streams {
		if probe.Streams[index].CodecType == "video" {
			return &probe.Streams[index]
		}
	}
	return nil
}

func (probe mediaProbe) audio() *probeStream {
	for index := range probe.Streams {
		if probe.Streams[index].CodecType == "audio" {
			return &probe.Streams[index]
		}
	}
	return nil
}

func (probe mediaProbe) duration() float64 {
	value, _ := strconv.ParseFloat(probe.Format.Duration, 64)
	return value
}

func (probe mediaProbe) videoSignature() string {
	stream := probe.video()
	if stream == nil {
		return ""
	}
	return strings.Join([]string{stream.CodecName, strconv.Itoa(stream.Width), strconv.Itoa(stream.Height),
		stream.PixFmt, orDefault(stream.SampleAspectRatio, "1:1"), orDefault(stream.ColorTransfer, "unknown"),
		orDefault(stream.ColorPrimaries, "unknown")}, "|")
}

func (probe mediaProbe) audioSignature() string {
	stream := probe.audio()
	if stream == nil {
		return "none"
	}
	return strings.Join([]string{stream.CodecName, orDefault(stream.Profile, "unknown"), stream.SampleRate,
		strconv.Itoa(stream.Channels), stream.ChannelLayout, orDefault(stream.ExtradataHash, "unknown")}, "|")
}

func (probe mediaProbe) videoCodec() string {
	if stream := probe.video(); stream != nil {
		return stream.CodecName
	}
	return ""
}

func (probe mediaProbe) hasAudio() bool { return probe.audio() != nil }

func orDefault(value, fallback string) string {
	if value == "" {
		return fallback
	}
	return value
}

type executor struct {
	ffmpeg  string
	ffprobe string
}

var probeArguments = []string{"-v", "error", "-hide_banner", "-print_format", "json",
	"-show_format", "-show_streams", "-show_data_hash", "sha256",
	"-protocol_whitelist", "file,crypto,data", "-i"}

func (media *executor) probe(ctx context.Context, file string) (mediaProbe, error) {
	arguments := append(append([]string{}, probeArguments...), file)
	body, err := media.execute(ctx, media.ffprobe, arguments)
	if err != nil {
		shortened := []string{}
		for index := 0; index < len(arguments); index++ {
			if arguments[index] == "-show_data_hash" {
				index++
				continue
			}
			shortened = append(shortened, arguments[index])
		}
		body, err = media.execute(ctx, media.ffprobe, shortened)
		if err != nil {
			return mediaProbe{}, err
		}
	}
	var probe mediaProbe
	if json.Unmarshal(body, &probe) != nil {
		return mediaProbe{}, errors.New("无法读取本地视频信息，请检查文件完整性")
	}
	if probe.video() == nil || probe.duration() <= 0 {
		return probe, errors.New("本地视频缺少视频轨或时长，请检查文件完整性")
	}
	return probe, nil
}

func (media *executor) execute(ctx context.Context, binary string, arguments []string) ([]byte, error) {
	command := exec.CommandContext(ctx, binary, arguments...)
	var stdout, stderr bytes.Buffer
	command.Stdout = &stdout
	command.Stderr = &stderr
	if err := command.Run(); err != nil {
		message := strings.TrimSpace(lastLine(stderr.String()))
		if message == "" {
			message = err.Error()
		}
		return nil, fmt.Errorf("%s 执行失败：%s", filepath.Base(binary), message)
	}
	return stdout.Bytes(), nil
}

func lastLine(value string) string {
	lines := strings.Split(strings.TrimRight(value, "\n"), "\n")
	if len(lines) == 0 {
		return ""
	}
	return lines[len(lines)-1]
}

func (media *executor) run(ctx context.Context, arguments []string, duration float64, progress func(float64)) error {
	base := []string{"-hide_banner", "-loglevel", "error", "-nostdin", "-y",
		"-progress", "pipe:1", "-nostats", "-protocol_whitelist", "file,crypto,data"}
	command := exec.CommandContext(ctx, media.ffmpeg, append(base, arguments...)...)
	stdout, err := command.StdoutPipe()
	if err != nil {
		return err
	}
	var stderr bytes.Buffer
	command.Stderr = &stderr
	if err := command.Start(); err != nil {
		return fmt.Errorf("无法启动 ffmpeg：%v", err)
	}
	reader := bufio.NewScanner(stdout)
	reader.Buffer(make([]byte, 64*1024), 64*1024)
	for reader.Scan() {
		line := strings.TrimSpace(reader.Text())
		if !strings.HasPrefix(line, "out_time_us=") && !strings.HasPrefix(line, "out_time_ms=") {
			continue
		}
		value, parseErr := strconv.ParseInt(line[strings.Index(line, "=")+1:], 10, 64)
		if parseErr != nil || duration <= 0 || progress == nil {
			continue
		}
		progress(math.Min(1, math.Max(0, float64(value)/1e6/duration)))
	}
	if waitErr := command.Wait(); waitErr != nil {
		message := strings.TrimSpace(lastLine(stderr.String()))
		if ctx.Err() != nil {
			return errors.New("已取消本地媒体处理")
		}
		if message == "" {
			message = waitErr.Error()
		}
		return fmt.Errorf("本地媒体处理失败（%s），原分集已保留", message)
	}
	return nil
}

func verifyDuration(probe mediaProbe, expected float64) error {
	value := probe.duration()
	if probe.videoCodec() == "" || math.IsNaN(value) || math.IsInf(value, 0) || value <= 0 ||
		expected > 0 && math.Abs(value-expected) > math.Max(2, expected*0.02) {
		return errors.New("生成的视频时长或视频轨不完整，原分集已保留")
	}
	return nil
}

func concatFileLine(file string) (string, error) {
	if strings.ContainsAny(file, "\n\r") {
		return "", errors.New("文件路径不能包含换行")
	}
	return "file '" + strings.ReplaceAll(file, "'", `'\''`) + "'", nil
}
