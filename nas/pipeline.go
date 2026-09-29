package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

type playbackPlan struct {
	Local bool   `json:"local"`
	URL   string `json:"url"`
	Key   string `json:"decryptionKey"`
}

func (manager *mergeManager) plan(drama dramaInput, index int) (playbackPlan, error) {
	data, err := coreCall("localPlayback", map[string]any{"drama": drama, "index": index})
	if err != nil {
		return playbackPlan{}, err
	}
	var plan playbackPlan
	if json.Unmarshal(data, &plan) != nil || !plan.Local || !filepath.IsAbs(plan.URL) {
		return playbackPlan{}, fmt.Errorf("第 %d 集没有完整的本地文件", index)
	}
	if _, err := os.Stat(plan.URL); err != nil {
		return playbackPlan{}, fmt.Errorf("第 %d 集没有完整的本地文件", index)
	}
	return plan, nil
}

func (manager *mergeManager) execute(ctx context.Context, id string) {
	manager.mu.Lock()
	job := manager.jobs[id]
	manager.mu.Unlock()
	if job == nil {
		manager.finish(id)
		return
	}
	work, cancel := context.WithCancel(ctx)
	manager.mu.Lock()
	if manager.cancels == nil {
		manager.cancels = map[string]context.CancelFunc{}
	}
	manager.cancels[id] = cancel
	manager.mu.Unlock()
	defer func() {
		cancel()
		manager.mu.Lock()
		delete(manager.cancels, id)
		manager.run = ""
		manager.mu.Unlock()
	}()
	item, err := manager.merge(work, job)
	manager.mu.Lock()
	if current := manager.jobs[id]; current != nil {
		if err != nil {
			current.State, current.Error, current.Stage = "failed", err.Error(), "合并未完成"
			if work.Err() != nil {
				current.State, current.Error = "queued", ""
			}
		} else {
			current.State, current.Progress, current.Stage, current.Error = "done", 1, "合并完成", ""
			current.Item = item
		}
	}
	_ = manager.saveLocked()
	manager.mu.Unlock()
	if err != nil {
		log.Println("合并任务", id, "未完成：", err)
		return
	}
	_ = os.RemoveAll(filepath.Join(manager.root, "nas-merge-work", id))
}

func (manager *mergeManager) finish(id string) {
	manager.mu.Lock()
	manager.run = ""
	manager.mu.Unlock()
}

func (manager *mergeManager) stage(id string, stage string, progress float64) {
	manager.mu.Lock()
	if job := manager.jobs[id]; job != nil && job.State == "running" {
		job.Stage, job.Progress = stage, progress
	}
	manager.mu.Unlock()
}

func (manager *mergeManager) merge(ctx context.Context, job *mergeState) (*libraryItem, error) {
	records, err := manager.app.jobs()
	if err != nil {
		return nil, err
	}
	byID := map[string]snapshotJob{}
	for _, record := range records {
		byID[record.ID] = record
	}
	selected := make([]snapshotJob, 0, len(job.JobIDs))
	for _, id := range job.JobIDs {
		record, found := byID[id]
		if !found || record.State != "completed" {
			return nil, errors.New("原分集已删除或未完成，请重新建立合并任务；已保存的处理进度保留")
		}
		if expected := job.Versions[id]; expected != "" && record.version() != expected {
			return nil, errors.New("原分集已删除、重新下载或未完成，请重新建立合并任务；已保存的处理进度保留")
		}
		selected = append(selected, record)
	}
	drama, _, err := continuousMergeJobs(selected)
	if err != nil {
		return nil, err
	}
	sorted := append([]snapshotJob{}, selected...)
	sortJobs(sorted)
	if !manager.lease(true) {
		return nil, errors.New("已有本地媒体任务正在运行，请稍后重试")
	}
	defer manager.lease(false)
	workspace := filepath.Join(manager.root, "nas-merge-work", job.ID)
	if err := os.MkdirAll(workspace, 0700); err != nil {
		return nil, errors.New("无法创建合并工作目录")
	}
	plans := make([]playbackPlan, len(sorted))
	sources := make([]any, 0, len(sorted))
	for index, record := range sorted {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		local, err := manager.plan(drama, record.Index)
		if err != nil {
			return nil, err
		}
		plans[index] = local
		digest := sha256.Sum256([]byte(local.Key))
		info, statErr := os.Stat(local.URL)
		var size, modified int64
		if statErr == nil {
			size, modified = info.Size(), info.ModTime().UnixNano()
		}
		sources = append(sources, []any{record.ID, record.version(), record.Index, filepath.Base(local.URL),
			size, modified, hex.EncodeToString(digest[:])})
	}
	fingerprint := digestOf([]any{"merge-v3", sources})
	checkpoint, err := loadCheckpoint(workspace, fingerprint)
	if err != nil {
		return nil, err
	}
	probes := make([]mediaProbe, len(sorted))
	prepared := make([]string, len(sorted))
	for index, record := range sorted {
		target := filepath.Join(workspace, "original-"+strconv.Itoa(index)+".mkv")
		probe, restored := manager.restored(ctx, checkpoint.Prepared, record.ID, target)
		if !restored {
			manager.stage(job.ID, "读取第 "+strconv.Itoa(record.Index)+" 集", 0.02*float64(index)/float64(len(sorted)))
			probe, err = manager.prepare(ctx, plans[index], target, record.Index)
			if err != nil {
				return nil, err
			}
			if err := recordCheckpoint(checkpoint, workspace, checkpoint.Prepared, record.ID, target, probe); err != nil {
				return nil, err
			}
		}
		probes[index] = probe
		prepared[index] = target
	}
	plan, err := createMergePlan(probes)
	if err != nil {
		return nil, err
	}
	planKey := digestOf([]any{plan.Video.videoSignature(), audioSignature(plan), plan.VideoChanges,
		plan.AudioChanges, countTrue(plan.VideoChanges), countTrue(plan.AudioChanges), plan.CanonicalAac, plan.TransportStream})
	if checkpoint.Plan != planKey {
		checkpoint.Normalized = map[string]checkpointRecord{}
		checkpoint.Plan = planKey
		if err := writeCheckpoint(workspace, checkpoint); err != nil {
			return nil, err
		}
	}
	extension := "mkv"
	if plan.TransportStream {
		extension = "ts"
	}
	normalized := make([]string, len(sorted))
	total := 0.0
	for _, probe := range probes {
		total += probe.duration()
	}
	for index, record := range sorted {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		target := filepath.Join(workspace, "part-"+strconv.Itoa(index)+"."+extension)
		probe, restored := manager.restored(ctx, checkpoint.Normalized, record.ID, target)
		if !restored {
			stage := "保留第 " + strconv.Itoa(record.Index) + " 集码流"
			if plan.VideoChanges[index] {
				stage = "统一第 " + strconv.Itoa(record.Index) + " 集视频格式"
			} else if plan.AudioChanges[index] {
				stage = "统一第 " + strconv.Itoa(record.Index) + " 集音轨"
			}
			base := 0.2 + 0.5*float64(index)/float64(len(sorted))
			manager.stage(job.ID, stage, base)
			weight := 0.5 / float64(len(sorted))
			if err := manager.media.run(ctx, plan.normalizeArguments(prepared[index], target, probes[index], index),
				probes[index].duration(), func(value float64) { manager.stage(job.ID, stage, base+weight*value) }); err != nil {
				return nil, err
			}
			probe, err = manager.media.probe(ctx, target)
			if err != nil {
				return nil, err
			}
			if err := verifyDuration(probe, probes[index].duration()); err != nil {
				return nil, err
			}
			if err := recordCheckpoint(checkpoint, workspace, checkpoint.Normalized, record.ID, target, probe); err != nil {
				return nil, err
			}
		}
		normalized[index] = target
	}
	manager.stage(job.ID, "合并视频", 0.7)
	lines := make([]string, 0, len(normalized))
	for _, file := range normalized {
		line, err := concatFileLine(file)
		if err != nil {
			return nil, err
		}
		lines = append(lines, line)
	}
	list := filepath.Join(workspace, "concat.txt")
	if err := writeFile(list, []byte("ffconcat version 1.0\n"+strings.Join(lines, "\n")+"\n")); err != nil {
		return nil, err
	}
	output := filepath.Join(workspace, "full.mkv")
	if err := manager.media.run(ctx, []string{"-f", "concat", "-safe", "0", "-i", list,
		"-map", "0:v:0", "-map", "0:a:0?", "-c", "copy", "-avoid_negative_ts", "make_zero", output},
		total, func(value float64) { manager.stage(job.ID, "合并视频", 0.7+0.18*value) }); err != nil {
		return nil, err
	}
	checked, err := manager.media.probe(ctx, output)
	if err != nil {
		return nil, err
	}
	if err := verifyDuration(checked, total); err != nil {
		return nil, err
	}
	manager.stage(job.ID, "完整解码校验成品", 0.88)
	if err := manager.decode(ctx, output, checked.duration()); err != nil {
		return nil, err
	}
	name := safeName(drama.Title) + " 第" + strconv.Itoa(job.Episodes[0]) + "-" +
		strconv.Itoa(job.Episodes[len(job.Episodes)-1]) + "集.mkv"
	relative := filepath.Join(job.ID, name)
	target := filepath.Join(manager.dir, relative)
	if err := os.MkdirAll(filepath.Dir(target), 0700); err != nil {
		return nil, errors.New("无法创建成品目录")
	}
	if err := os.Rename(output, target); err != nil {
		return nil, errors.New("成品移动到库目录失败，原分集已保留")
	}
	item := &libraryItem{ID: job.ID, Drama: drama, File: relative, Kind: "merged", Episodes: job.Episodes,
		Duration: checked.duration(), Bytes: fileSize(target), Created: time.Now(),
		VideoTranscodes: plan.videoTranscodes(), AudioTranscodes: plan.audioTranscodes(),
		DecodeVerified: true, SourceVersion: fingerprint}
	if err := manager.library.add(item); err != nil {
		return nil, err
	}
	return item, nil
}

func (manager *mergeManager) prepare(ctx context.Context, plan playbackPlan, target string, index int) (mediaProbe, error) {
	arguments := []string{}
	if plan.Key != "" {
		arguments = append(arguments, "-decryption_key", plan.Key)
	}
	if strings.EqualFold(filepath.Ext(plan.URL), ".m3u8") {
		arguments = append(arguments, "-allowed_extensions", "ALL", "-extension_picky", "0")
	}
	arguments = append(arguments, "-i", plan.URL, "-map", "0:v:0", "-map", "0:a:0?", "-c", "copy",
		"-map_metadata", "-1", "-avoid_negative_ts", "make_zero", target)
	if err := manager.media.run(ctx, arguments, 0, nil); err != nil {
		return mediaProbe{}, err
	}
	probe, err := manager.media.probe(ctx, target)
	if err != nil {
		return mediaProbe{}, err
	}
	if err := verifyDuration(probe, 0); err != nil {
		return mediaProbe{}, err
	}
	return probe, nil
}

func (manager *mergeManager) decode(ctx context.Context, file string, duration float64) error {
	return manager.media.run(ctx, []string{"-xerror", "-err_detect", "explode", "-i", file,
		"-map", "0:v:0", "-map", "0:a:0?", "-sn", "-dn", "-f", "null", "-"},
		duration, func(value float64) { manager.stageCurrent(0.88 + 0.09*value) })
}

func (manager *mergeManager) stageCurrent(progress float64) {
	manager.mu.Lock()
	if job := manager.jobs[manager.run]; job != nil && job.State == "running" {
		job.Progress = progress
	}
	manager.mu.Unlock()
}

func (manager *mergeManager) restored(ctx context.Context, records map[string]checkpointRecord, id string, file string) (mediaProbe, bool) {
	saved, found := records[id]
	if !found {
		return mediaProbe{}, false
	}
	info, err := os.Stat(file)
	if err != nil || !info.Mode().IsRegular() || info.Size() != saved.Bytes || info.ModTime().UnixNano() != saved.Modified {
		return mediaProbe{}, false
	}
	probe, err := manager.media.probe(ctx, file)
	if err != nil || verifyDuration(probe, saved.Duration) != nil {
		return mediaProbe{}, false
	}
	if probe.videoSignature() != saved.Video || probe.audioSignature() != saved.Audio {
		return mediaProbe{}, false
	}
	return probe, true
}

func (manager *mergeManager) lease(start bool) bool {
	command := "end"
	if start {
		command = "start"
	}
	if _, err := coreCall("workLease", map[string]any{"jobId": "media", "command": command}); err != nil {
		return !start
	}
	return true
}

func loadCheckpoint(workspace string, fingerprint string) (*checkpointFile, error) {
	value := &checkpointFile{Fingerprint: fingerprint,
		Prepared: map[string]checkpointRecord{}, Normalized: map[string]checkpointRecord{}}
	body, err := os.ReadFile(filepath.Join(workspace, "checkpoint.json"))
	if errors.Is(err, os.ErrNotExist) {
		return value, writeCheckpoint(workspace, value)
	}
	if err != nil || len(body) > 8<<20 {
		return nil, errors.New("合并恢复记录过大或无法读取，已保留原文件")
	}
	var stored checkpointFile
	if json.Unmarshal(body, &stored) != nil {
		return nil, errors.New("合并恢复记录无法解析，原视频保留")
	}
	if stored.Fingerprint != fingerprint {
		return nil, errors.New("原分集或合并参数已改变，请取消此任务后重新加入；原视频保留")
	}
	if stored.Prepared == nil {
		stored.Prepared = map[string]checkpointRecord{}
	}
	if stored.Normalized == nil {
		stored.Normalized = map[string]checkpointRecord{}
	}
	return &stored, nil
}

func recordCheckpoint(checkpoint *checkpointFile, workspace string, records map[string]checkpointRecord,
	id string, file string, probe mediaProbe) error {
	info, err := os.Stat(file)
	if err != nil {
		return err
	}
	records[id] = checkpointRecord{Bytes: info.Size(), Modified: info.ModTime().UnixNano(),
		Duration: probe.duration(), Video: probe.videoSignature(), Audio: probe.audioSignature()}
	return writeCheckpoint(workspace, checkpoint)
}

func writeCheckpoint(workspace string, checkpoint *checkpointFile) error {
	body, err := json.Marshal(checkpoint)
	if err != nil {
		return err
	}
	return writeFile(filepath.Join(workspace, "checkpoint.json"), body)
}

func writeFile(path string, body []byte) error {
	temporary := path + ".writing"
	if err := os.WriteFile(temporary, body, 0600); err != nil {
		return err
	}
	return os.Rename(temporary, path)
}
