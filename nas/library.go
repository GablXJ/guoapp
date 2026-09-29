package main

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

type libraryItem struct {
	ID              string      `json:"id"`
	Drama           dramaInput  `json:"drama"`
	File            string      `json:"file"`
	Kind            string      `json:"kind"`
	Episodes        []int       `json:"episodes"`
	Duration        float64     `json:"duration"`
	Bytes           int64       `json:"bytes"`
	Created         time.Time   `json:"created"`
	VideoTranscodes int         `json:"videoTranscodes"`
	AudioTranscodes int         `json:"audioTranscodes"`
	DecodeVerified  bool        `json:"decodeVerified"`
	SourceVersion   string      `json:"sourceVersion"`
	Exports         []exportRef `json:"exports,omitempty"`
}

type exportRef struct {
	Folder string `json:"folder"`
	File   string `json:"file"`
	Number int    `json:"number"`
}

type libraryStore struct {
	mu        sync.Mutex
	directory string
	path      string
	items     []*libraryItem
}

func (store *libraryStore) load() error {
	store.path = filepath.Join(store.directory, "index.json")
	body, err := os.ReadFile(store.path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil || len(body) > 16<<20 {
		return errors.New("合并成品记录无法读取，原文件已保留")
	}
	var items []*libraryItem
	if json.Unmarshal(body, &items) != nil {
		return errors.New("合并成品记录无法解析，原文件已保留")
	}
	for _, item := range items {
		if item == nil || item.ID == "" || !safeRelative(item.File) {
			return errors.New("合并成品记录条目无效，原文件已保留")
		}
	}
	store.items = items
	return nil
}

func safeRelative(value string) bool {
	if value == "" || filepath.IsAbs(value) {
		return false
	}
	for _, part := range strings.Split(filepath.ToSlash(value), "/") {
		if part == "" || part == "." || part == ".." {
			return false
		}
	}
	return true
}

func (store *libraryStore) saveLocked() error {
	body, err := json.MarshalIndent(store.items, "", " ")
	if err != nil {
		return err
	}
	return writeFile(store.path, body)
}

func (store *libraryStore) add(item *libraryItem) error {
	store.mu.Lock()
	defer store.mu.Unlock()
	for index, candidate := range store.items {
		if candidate.ID == item.ID {
			store.items[index] = item
			return store.saveLocked()
		}
	}
	store.items = append(store.items, item)
	return store.saveLocked()
}

func (store *libraryStore) update(item *libraryItem) error {
	return store.add(item)
}

func (store *libraryStore) remove(id string) (*libraryItem, error) {
	store.mu.Lock()
	defer store.mu.Unlock()
	for index, candidate := range store.items {
		if candidate.ID != id {
			continue
		}
		store.items = append(append([]*libraryItem{}, store.items[:index]...), store.items[index+1:]...)
		if err := store.saveLocked(); err != nil {
			return nil, err
		}
		return candidate, nil
	}
	return nil, nil
}

func (store *libraryStore) item(id string) *libraryItem {
	store.mu.Lock()
	defer store.mu.Unlock()
	for _, candidate := range store.items {
		if candidate.ID == id {
			return candidate
		}
	}
	return nil
}

func (store *libraryStore) list() []*libraryItem {
	store.mu.Lock()
	defer store.mu.Unlock()
	items := append([]*libraryItem{}, store.items...)
	sort.SliceStable(items, func(i, j int) bool { return items[i].Created.After(items[j].Created) })
	return items
}

func digestOf(value any) string {
	body, _ := json.Marshal(value)
	sum := sha256.Sum256(body)
	return hex.EncodeToString(sum[:])
}

func fileSize(path string) int64 {
	info, err := os.Stat(path)
	if err != nil {
		return 0
	}
	return info.Size()
}

func safeName(name string) string {
	builder := strings.Builder{}
	for _, character := range name {
		if character < 0x20 || strings.ContainsRune("<>:\"/\\|?*", character) {
			builder.WriteRune('_')
			continue
		}
		builder.WriteRune(character)
	}
	result := strings.TrimRight(strings.TrimSpace(builder.String()), ". ")
	if result == "" {
		return "短剧"
	}
	runes := []rune(result)
	if len(runes) > 60 {
		return string(runes[:60])
	}
	return result
}

func sortJobs(records []snapshotJob) {
	sort.SliceStable(records, func(i, j int) bool { return records[i].Index < records[j].Index })
}

func (manager *mergeManager) control(id string, command string) error {
	manager.mu.Lock()
	job := manager.jobs[id]
	cancel := manager.cancels[id]
	if job == nil {
		manager.mu.Unlock()
		return errors.New("合并任务不存在，请刷新列表")
	}
	switch command {
	case "cancel":
		if job.State != "running" {
			manager.mu.Unlock()
			return nil
		}
		job.State, job.Stage, job.Error, job.Progress = "queued", "已取消", "", 0
	case "retry":
		if job.State == "done" {
			manager.mu.Unlock()
			return errors.New("该任务已完成")
		}
		job.State, job.Stage, job.Error, job.Progress = "queued", "等待重新合并", "", 0
	case "remove":
		if job.State == "done" {
			manager.mu.Unlock()
			return errors.New("请先删除成品再移除任务")
		}
		delete(manager.jobs, id)
		kept := manager.order[:0]
		for _, candidate := range manager.order {
			if candidate != id {
				kept = append(kept, candidate)
			}
		}
		manager.order = kept
	default:
		manager.mu.Unlock()
		return errors.New("不支持的合并操作")
	}
	err := manager.saveLocked()
	manager.mu.Unlock()
	if cancel != nil && command != "retry" {
		cancel()
	}
	if command == "remove" {
		_ = os.RemoveAll(filepath.Join(manager.root, "nas-merge-work", id))
	}
	return err
}

func (manager *mergeManager) deleteItem(id string, removeFile bool) error {
	item := manager.library.item(id)
	if item == nil {
		return errors.New("成品不存在，请刷新列表")
	}
	if !removeFile {
		manager.mu.Lock()
		delete(manager.jobs, id)
		kept := manager.order[:0]
		for _, candidate := range manager.order {
			if candidate != id {
				kept = append(kept, candidate)
			}
		}
		manager.order = kept
		err := manager.saveLocked()
		manager.mu.Unlock()
		return err
	}
	path := filepath.Join(manager.dir, item.File)
	if _, err := os.Stat(path); err == nil {
		if err := os.Remove(path); err != nil {
			return errors.New("成品正在使用或无法删除，请稍后重试")
		}
	}
	_ = os.RemoveAll(filepath.Dir(path))
	if _, err := manager.library.remove(id); err != nil {
		return err
	}
	return manager.control(id, "remove")
}

func (manager *mergeManager) export(id string) (string, error) {
	item := manager.library.item(id)
	if item == nil {
		return "", errors.New("合并成品不存在，请刷新后重试")
	}
	source := filepath.Join(manager.dir, item.File)
	info, err := os.Stat(source)
	if err != nil || info.Size() != item.Bytes {
		return "", errors.New("合并成品缺失或不完整，请重新合并")
	}
	hash := digestOf(item.Drama.ID)
	folder := filepath.Join(manager.root, "nas-exports",
		safeName(item.Drama.Title)+" ["+dramaSource(item.Drama)+"-"+hash[:12]+"]")
	if !filepath.IsAbs(folder) {
		return "", errors.New("导出目录无效")
	}
	number := 1
	for _, previous := range item.Exports {
		if previous.Number >= number {
			number = previous.Number + 1
		}
	}
	if number > 100000 {
		return "", errors.New("导出次数已达上限")
	}
	season := filepath.Join(folder, "Season 00")
	if err := os.MkdirAll(season, 0700); err != nil {
		return "", errors.New("无法创建导出目录")
	}
	name := fmt.Sprintf("%s 第%d-%d集 S00E%s.mkv", safeName(item.Drama.Title),
		item.Episodes[0], item.Episodes[len(item.Episodes)-1], padNumber(number))
	target := filepath.Join(season, name)
	if err := copyFile(source, target, item.Bytes); err != nil {
		return "", err
	}
	poster := filepath.Join(folder, "poster.jpg")
	hasPoster := false
	if _, err := os.Stat(poster); err != nil {
		if cover, coverErr := manager.app.coverPath(item.Drama); coverErr == nil && cover != "" {
			if copyFile(cover, poster, fileSize(cover)) == nil {
				hasPoster = true
			}
		}
	} else {
		hasPoster = true
	}
	if err := writeFile(filepath.Join(folder, "tvshow.nfo"), []byte(showNFO(item.Drama, hasPoster))); err != nil {
		return "", err
	}
	if err := writeFile(strings.TrimSuffix(target, filepath.Ext(target))+".nfo",
		[]byte(specialNFO(item.Drama, item, number))); err != nil {
		return "", err
	}
	relative, _ := filepath.Rel(manager.root, target)
	item.Exports = append(item.Exports, exportRef{Folder: filepath.Base(folder), File: relative, Number: number})
	if err := manager.library.add(item); err != nil {
		return "", err
	}
	return filepath.ToSlash(relative), nil
}

func padNumber(number int) string {
	text := strconv.Itoa(number)
	for len(text) < 3 {
		text = "0" + text
	}
	return text
}

func xmlText(value string) string {
	builder := strings.Builder{}
	for _, character := range value {
		if character < 0x20 && character != '\t' && character != '\n' {
			continue
		}
		switch character {
		case '&':
			builder.WriteString("&amp;")
		case '<':
			builder.WriteString("&lt;")
		case '>':
			builder.WriteString("&gt;")
		default:
			builder.WriteRune(character)
		}
	}
	return builder.String()
}

func showNFO(drama dramaInput, localPoster bool) string {
	cover := ""
	if !localPoster {
		address, err := url.Parse(drama.Cover)
		if err == nil && (address.Scheme == "https" || address.Scheme == "http") {
			cover = drama.Cover
		}
	}
	thumb := ""
	if localPoster {
		thumb = "<thumb aspect=\"poster\">poster.jpg</thumb>"
	} else if cover != "" {
		thumb = "<thumb aspect=\"poster\">" + xmlText(cover) + "</thumb>"
	}
	return xmlHeader() + "<tvshow><title>" + xmlText(drama.Title) + "</title><plot>" +
		xmlText(drama.Description) + "</plot><uniqueid type=\"zhenguojian\" default=\"true\">" +
		xmlText(drama.ID) + "</uniqueid>" + thumb + "<season>1</season><episode>" +
		strconv.Itoa(drama.Episodes) + "</episode></tvshow>\n"
}

func specialNFO(drama dramaInput, item *libraryItem, number int) string {
	return xmlHeader() + "<episodedetails><title>" + xmlText(drama.Title) + " · 合并第 " +
		strconv.Itoa(item.Episodes[0]) + "–" + strconv.Itoa(item.Episodes[len(item.Episodes)-1]) + " 集</title>" +
		"<showtitle>" + xmlText(drama.Title) + "</showtitle><season>0</season><episode>" +
		strconv.Itoa(number) + "</episode><displayseason>1</displayseason><displayepisode>" +
		strconv.Itoa(item.Episodes[len(item.Episodes)-1]+1) + "</displayepisode><plot>" +
		xmlText("合并第 "+strconv.Itoa(item.Episodes[0])+"–"+strconv.Itoa(item.Episodes[len(item.Episodes)-1])+
			" 集，共 "+strconv.Itoa(len(item.Episodes))+" 集。\n"+drama.Description) + "</plot>" +
		"<uniqueid type=\"zhenguojian\" default=\"true\">" + xmlText(item.ID) + "</uniqueid></episodedetails>\n"
}

func xmlHeader() string { return "<?xml version=\"1.0\" encoding=\"utf-8\"?>\n" }

func copyFile(source string, target string, expected int64) error {
	input, err := os.Open(source)
	if err != nil {
		return errors.New("合并成品缺失或不完整，请重新合并")
	}
	defer input.Close()
	if err := os.MkdirAll(filepath.Dir(target), 0700); err != nil {
		return errors.New("无法创建导出目录")
	}
	staged := target + ".writing"
	output, err := os.OpenFile(staged, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0600)
	if err != nil {
		return err
	}
	copied, copyErr := io.Copy(output, input)
	if copyErr == nil {
		copyErr = output.Sync()
	}
	_ = output.Close()
	if copyErr != nil || copied != expected {
		_ = os.Remove(staged)
		if copyErr != nil {
			return copyErr
		}
		return errors.New("复制成品时文件发生变化，请重试；原成品保留")
	}
	return os.Rename(staged, target)
}
