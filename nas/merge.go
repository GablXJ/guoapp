package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

type mergeState struct {
	ID       string            `json:"id"`
	Drama    dramaInput        `json:"drama"`
	JobIDs   []string          `json:"jobIds"`
	Versions map[string]string `json:"versions"`
	Stage    string            `json:"stage"`
	State    string            `json:"state"`
	Progress float64           `json:"progress"`
	Error    string            `json:"error"`
	Created  int64             `json:"created"`
	Item     *libraryItem      `json:"item,omitempty"`
	Episodes []int             `json:"episodes"`
}

type mergeManager struct {
	app     *server
	root    string
	media   *executor
	dir     string
	mu      sync.Mutex
	jobs    map[string]*mergeState
	order   []string
	cancels map[string]context.CancelFunc
	queue   string
	run     string
	halt    chan struct{}
	done    chan struct{}
	library *libraryStore
}

type checkpointRecord struct {
	Bytes    int64   `json:"bytes"`
	Modified int64   `json:"modified"`
	Duration float64 `json:"duration"`
	Video    string  `json:"video"`
	Audio    string  `json:"audio"`
}

type checkpointFile struct {
	Fingerprint string                      `json:"fingerprint"`
	Plan        string                      `json:"plan"`
	Prepared    map[string]checkpointRecord `json:"prepared"`
	Normalized  map[string]checkpointRecord `json:"normalized"`
}

func newMergeManager(app *server, root string) (*mergeManager, error) {
	manager := &mergeManager{
		app: app, root: root, dir: filepath.Join(root, "nas-merged"),
		media: &executor{ffmpeg: app.settings.FFmpeg, ffprobe: app.settings.FFprobe},
		jobs:  map[string]*mergeState{},
		halt:  make(chan struct{}), done: make(chan struct{}),
	}
	if err := os.MkdirAll(manager.dir, 0700); err != nil {
		return nil, errors.New("无法创建合并成品目录")
	}
	manager.queue = filepath.Join(manager.dir, "queue.json")
	body, err := os.ReadFile(manager.queue)
	if err == nil && len(body) < 8<<20 {
		var restored []*mergeState
		if json.Unmarshal(body, &restored) == nil {
			for _, job := range restored {
				if job == nil || job.ID == "" {
					continue
				}
				if job.State == "running" {
					job.State, job.Progress, job.Error = "queued", 0, ""
				}
				if job.State == "queued" || job.State == "failed" {
					manager.jobs[job.ID] = job
					manager.order = append(manager.order, job.ID)
				}
			}
		}
	}
	manager.library = &libraryStore{directory: manager.dir}
	if err := manager.library.load(); err != nil {
		return nil, err
	}
	return manager, nil
}

func (manager *mergeManager) serve(ctx context.Context) {
	defer close(manager.done)
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-manager.halt:
			return
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
		id := manager.next()
		if id == "" {
			continue
		}
		manager.execute(ctx, id)
	}
}

func (manager *mergeManager) next() string {
	manager.mu.Lock()
	defer manager.mu.Unlock()
	if manager.run != "" {
		return ""
	}
	for _, id := range manager.order {
		if job := manager.jobs[id]; job != nil && job.State == "queued" {
			manager.run = id
			job.State, job.Stage, job.Error, job.Progress = "running", "准备合并", "", 0
			return id
		}
	}
	return ""
}

func (manager *mergeManager) stop() {
	close(manager.halt)
	manager.mu.Lock()
	current := manager.run
	manager.mu.Unlock()
	if current != "" {
		_ = manager.control(current, "cancel")
	}
	select {
	case <-manager.done:
	case <-time.After(15 * time.Second):
	}
	_ = manager.save()
}

func (manager *mergeManager) running() string {
	manager.mu.Lock()
	defer manager.mu.Unlock()
	return manager.run
}

func (manager *mergeManager) snapshot() []*mergeState {
	manager.mu.Lock()
	defer manager.mu.Unlock()
	items := make([]*mergeState, 0, len(manager.order))
	for index := len(manager.order) - 1; index >= 0; index-- {
		if job := manager.jobs[manager.order[index]]; job != nil {
			items = append(items, job)
		}
	}
	return items
}

func (manager *mergeManager) save() error {
	manager.mu.Lock()
	defer manager.mu.Unlock()
	return manager.saveLocked()
}

func (manager *mergeManager) saveLocked() error {
	items := make([]*mergeState, 0, len(manager.order))
	for _, id := range manager.order {
		if job := manager.jobs[id]; job != nil && (job.State == "queued" || job.State == "running" || job.State == "failed") {
			items = append(items, job)
		}
	}
	body, err := json.Marshal(items)
	if err != nil {
		return err
	}
	temporary := manager.queue + ".writing"
	if err := os.WriteFile(temporary, body, 0600); err != nil {
		return err
	}
	return os.Rename(temporary, manager.queue)
}

func (manager *mergeManager) start(jobIDs []string) (*mergeState, error) {
	for _, tool := range []string{manager.app.settings.FFmpeg, manager.app.settings.FFprobe} {
		if _, err := exec.LookPath(tool); err != nil {
			return nil, fmt.Errorf("找不到 %s，合并无法开始；请使用自带 ffmpeg 的镜像，或在设置里填写可执行文件路径", filepath.Base(tool))
		}
	}
	records, err := manager.app.jobs()
	if err != nil {
		return nil, err
	}
	byID := map[string]snapshotJob{}
	for _, record := range records {
		byID[record.ID] = record
	}
	selected := make([]snapshotJob, 0, len(jobIDs))
	seen := map[string]bool{}
	for _, id := range jobIDs {
		record, found := byID[id]
		if !found || seen[id] {
			return nil, errors.New("下载任务不存在或重复")
		}
		seen[id] = true
		if record.State != "completed" {
			return nil, errors.New("只能合并已下载完成的分集")
		}
		selected = append(selected, record)
	}
	drama, episodes, err := continuousMergeJobs(selected)
	if err != nil {
		return nil, err
	}
	if !manager.app.sourceAllowed(dramaSource(drama)) {
		return nil, errors.New("当前版本不包含此站源")
	}
	versions := map[string]string{}
	for _, record := range selected {
		versions[record.ID] = record.version()
	}
	id, err := randomToken()
	if err != nil {
		return nil, errors.New("无法创建合并任务")
	}
	id = id[:32]
	job := &mergeState{ID: id, Drama: drama, JobIDs: jobIDs, Versions: versions, Episodes: episodes,
		State: "queued", Stage: "等待合并", Created: time.Now().UnixMilli()}
	manager.mu.Lock()
	if len(manager.order) >= 200 {
		manager.mu.Unlock()
		return nil, errors.New("合并队列已满，请先清理已完成任务")
	}
	manager.jobs[id] = job
	manager.order = append([]string{id}, manager.order...)
	err = manager.saveLocked()
	manager.mu.Unlock()
	if err != nil {
		return nil, err
	}
	return job, nil
}

func continuousMergeJobs(selected []snapshotJob) (dramaInput, []int, error) {
	if len(selected) < 2 || len(selected) > 10000 {
		return dramaInput{}, nil, errors.New("请选择同一部剧 2 至 10000 集已下载的视频")
	}
	sorted := append([]snapshotJob{}, selected...)
	sort.SliceStable(sorted, func(i, j int) bool { return sorted[i].Index < sorted[j].Index })
	for _, record := range sorted {
		if record.Drama.ID != sorted[0].Drama.ID {
			return dramaInput{}, nil, errors.New("请选择同一部剧的分集")
		}
	}
	numbers := []int{}
	seen := map[int]bool{}
	for _, record := range sorted {
		if seen[record.Index] {
			return dramaInput{}, nil, errors.New("集数不能重复")
		}
		seen[record.Index] = true
		numbers = append(numbers, record.Index)
	}
	missing := []int{}
	for index := 1; index < len(numbers); index++ {
		for number := numbers[index-1] + 1; number < numbers[index] && len(missing) < 12; number++ {
			missing = append(missing, number)
		}
	}
	if len(missing) > 0 {
		suffix := ""
		if len(missing) == 12 {
			suffix = "…"
		}
		return dramaInput{}, nil, errors.New("分集不连续，缺少第 " + strings.Join(intsToStrings(missing), "、") +
			suffix + " 集，请补齐后合并")
	}
	return sorted[0].Drama, numbers, nil
}

func intsToStrings(values []int) []string {
	out := make([]string, 0, len(values))
	for _, value := range values {
		out = append(out, strconv.Itoa(value))
	}
	return out
}
