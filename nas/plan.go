package main

import (
	"errors"
	"fmt"
	"strconv"
	"strings"
)

type mergePlan struct {
	Video           mediaProbe
	Audio           *mediaProbe
	VideoChanges    []bool
	AudioChanges    []bool
	TransportStream bool
	CanonicalAac    bool
}

func (plan *mergePlan) videoTranscodes() int { return countTrue(plan.VideoChanges) }

func (plan *mergePlan) audioTranscodes() int { return countTrue(plan.AudioChanges) }

func countTrue(values []bool) int {
	count := 0
	for _, value := range values {
		if value {
			count++
		}
	}
	return count
}

func audioSignature(plan *mergePlan) string {
	if plan.Audio == nil {
		return ""
	}
	return plan.Audio.audioSignature()
}

func colorTransfer(probe mediaProbe) string {
	if stream := probe.video(); stream != nil {
		return stream.ColorTransfer
	}
	return ""
}

func extradataHash(probe mediaProbe) string {
	if stream := probe.video(); stream != nil {
		return stream.ExtradataHash
	}
	return ""
}

func majorityProbe(probes []mediaProbe, signature func(mediaProbe) string) *mediaProbe {
	groups := map[string][]mediaProbe{}
	order := []string{}
	for _, probe := range probes {
		key := signature(probe)
		if _, found := groups[key]; !found {
			order = append(order, key)
		}
		groups[key] = append(groups[key], probe)
	}
	best := groups[order[0]][0]
	bestCount, bestTotal := 0, 0.0
	for _, key := range order {
		total := 0.0
		for _, probe := range groups[key] {
			total += probe.duration()
		}
		if len(groups[key]) > bestCount || len(groups[key]) == bestCount && total > bestTotal {
			best, bestCount, bestTotal = groups[key][0], len(groups[key]), total
		}
	}
	return &best
}

func createMergePlan(probes []mediaProbe) (*mergePlan, error) {
	if len(probes) < 2 {
		return nil, errors.New("请至少选择两集完整视频")
	}
	for _, probe := range probes {
		if probe.videoCodec() == "" || probe.duration() <= 0 {
			return nil, errors.New("请至少选择两集完整视频")
		}
	}
	video := majorityProbe(probes, mediaProbe.videoSignature)
	var withAudio []mediaProbe
	for _, probe := range probes {
		if probe.hasAudio() {
			withAudio = append(withAudio, probe)
		}
	}
	plan := &mergePlan{Video: *video}
	if len(withAudio) > 0 {
		plan.Audio = majorityProbe(withAudio, mediaProbe.audioSignature)
	}
	plan.VideoChanges = make([]bool, len(probes))
	plan.AudioChanges = make([]bool, len(probes))
	for index, probe := range probes {
		plan.VideoChanges[index] = probe.videoSignature() != video.videoSignature()
		plan.AudioChanges[index] = plan.Audio != nil && probe.audioSignature() != plan.Audio.audioSignature()
	}
	plan.CanonicalAac = plan.Audio != nil && plan.Audio.audio().CodecName == "aac" && countTrue(plan.AudioChanges) > 0
	if plan.CanonicalAac {
		for index := range plan.AudioChanges {
			plan.AudioChanges[index] = true
		}
	}
	if countTrue(plan.VideoChanges) > 0 && !contains([]string{"h264", "hevc"}, video.videoCodec()) {
		return nil, fmt.Errorf("多数分集为 %s，当前不能将其他格式转换为此编码；原文件已保留", video.videoCodec())
	}
	for index, probe := range probes {
		if !plan.VideoChanges[index] {
			continue
		}
		source, target := colorTransfer(probe), colorTransfer(*video)
		if (source == "smpte2084" || source == "arib-std-b67" || target == "smpte2084" || target == "arib-std-b67") &&
			source != target {
			return nil, errors.New("分集包含不同 HDR 色彩格式，暂不自动转换，避免改变画面色彩")
		}
	}
	if plan.Audio != nil && countTrue(plan.AudioChanges) > 0 &&
		!contains([]string{"aac", "ac3", "eac3", "flac", "alac", "mp2", "pcm_s16le", "pcm_s24le"}, plan.Audio.audio().CodecName) {
		return nil, errors.New("多数音轨格式暂不支持统一编码，可先按分集导出")
	}
	plan.TransportStream = contains([]string{"h264", "hevc"}, video.videoCodec()) &&
		(plan.Audio == nil || contains([]string{"aac", "ac3", "eac3", "mp2"}, plan.Audio.audio().CodecName))
	if !plan.TransportStream {
		target := extradataHash(*video)
		for _, probe := range probes {
			if extradataHash(probe) != target {
				return nil, errors.New("视频编码参数不适合直接拼接，可保留分集并按集下载")
			}
		}
	}
	return plan, nil
}

func contains(values []string, candidate string) bool {
	for _, value := range values {
		if value == candidate {
			return true
		}
	}
	return false
}

func (plan *mergePlan) normalizeArguments(input, output string, probe mediaProbe, index int) []string {
	arguments := []string{"-i", input}
	video := plan.Video.video()
	addSilence := plan.Audio != nil && !probe.hasAudio()
	if addSilence {
		audio := plan.Audio.audio()
		layout := orDefault(audio.ChannelLayout, "stereo")
		switch audio.Channels {
		case 1:
			layout = "mono"
		case 2:
			layout = "stereo"
		}
		arguments = append(arguments, "-f", "lavfi", "-i", "anullsrc=r="+orDefault(audio.SampleRate, "44100")+":cl="+layout)
	}
	arguments = append(arguments, "-map", "0:v:0")
	if plan.Audio != nil {
		if addSilence {
			arguments = append(arguments, "-map", "1:a:0")
		} else {
			arguments = append(arguments, "-map", "0:a:0")
		}
	}
	if plan.VideoChanges[index] {
		encoder := "libx264"
		if video.CodecName == "hevc" {
			encoder = "libx265"
		}
		ratio := strings.ReplaceAll(orDefault(video.SampleAspectRatio, "1:1"), ":", "/")
		arguments = append(arguments, "-c:v", encoder, "-preset", "veryfast", "-crf", "18", "-threads", "2",
			"-vf", fmt.Sprintf("scale=%d:%d:force_original_aspect_ratio=decrease,pad=%d:%d:(ow-iw)/2:(oh-ih)/2,setsar=%s,format=%s",
				video.Width, video.Height, video.Width, video.Height, ratio, orDefault(video.PixFmt, "yuv420p")))
		if video.CodecName == "hevc" {
			arguments = append(arguments, "-x265-params", "pools=2:frame-threads=1")
		}
	} else {
		arguments = append(arguments, "-c:v", "copy")
	}
	if plan.Audio != nil {
		audio := plan.Audio.audio()
		if plan.AudioChanges[index] {
			arguments = append(arguments, "-c:a", audio.CodecName, "-ar", audio.SampleRate, "-ac", strconv.Itoa(audio.Channels))
			if plan.CanonicalAac {
				arguments = append(arguments, "-profile:a", "aac_low")
			}
			if contains([]string{"aac", "ac3", "eac3", "mp2"}, audio.CodecName) {
				arguments = append(arguments, "-b:a", "192k")
			}
		} else {
			arguments = append(arguments, "-c:a", "copy")
		}
	}
	if addSilence {
		arguments = append(arguments, "-t", strconv.FormatFloat(probe.duration(), 'f', 6, 64))
	}
	arguments = append(arguments, "-map_metadata", "-1", "-avoid_negative_ts", "make_zero")
	if plan.TransportStream {
		bitstream := "h264_mp4toannexb"
		if video.CodecName == "hevc" {
			bitstream = "hevc_mp4toannexb"
		}
		arguments = append(arguments, "-bsf:v", bitstream, "-mpegts_flags", "+resend_headers", "-f", "mpegts")
	}
	return append(arguments, output)
}
