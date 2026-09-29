package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

type dramaInput struct {
	MetadataSchema int      `json:"metadataSchema"`
	ID             string   `json:"id"`
	Source         string   `json:"source"`
	SourceID       string   `json:"sourceId"`
	Title          string   `json:"title"`
	Description    string   `json:"description"`
	Cover          string   `json:"cover"`
	Episodes       int      `json:"episodes"`
	Category       string   `json:"category"`
	VIP            *bool    `json:"vip"`
	Heat           string   `json:"heat"`
	Views          string   `json:"views"`
	OnlineDate     string   `json:"onlineDate"`
	Tags           []string `json:"tags"`
	ReleaseStatus  string   `json:"releaseStatus"`
}

type chapterInput struct {
	ID             string          `json:"id"`
	Source         string          `json:"source"`
	Title          string          `json:"title"`
	CurrentEpisode json.RawMessage `json:"currentEpisode"`
	MediaSize      int64           `json:"mediaSize"`
}

type downloadEntry struct {
	Chapter chapterInput `json:"chapter"`
	Index   int          `json:"index"`
}

type resourceSettingsInput struct {
	ProxyMode           string `json:"proxyMode"`
	ProxyURL            string `json:"proxyUrl"`
	CatalogConcurrency  int    `json:"catalogConcurrency"`
	CatalogIntervalMS   int    `json:"catalogIntervalMs"`
	DownloadConcurrency int    `json:"downloadConcurrency"`
	DownloadBySource    bool   `json:"downloadBySource"`
}

type clientRequest struct {
	Action           string                `json:"action"`
	Source           string                `json:"source"`
	Category         string                `json:"category"`
	Query            string                `json:"query"`
	Board            string                `json:"board"`
	Index            int                   `json:"index"`
	Page             int                   `json:"page"`
	JobID            string                `json:"jobId"`
	Command          string                `json:"command"`
	Drama            dramaInput            `json:"drama"`
	Chapters         []chapterInput        `json:"chapters"`
	Entries          []downloadEntry       `json:"entries"`
	JobIDs           []string              `json:"jobIds"`
	Quality          int                   `json:"quality"`
	Force            bool                  `json:"force"`
	ExpectedVersions map[string]string     `json:"expectedVersions"`
	Settings         resourceSettingsInput `json:"settings"`
}

var rpcActions = map[string]bool{
	"catalog": true, "cached": true, "categories": true, "suggestions": true,
	"recommendations": true, "cachedRecommendations": true, "rankingBoards": true, "rankings": true,
	"detail": true, "metadata": true, "resolve": true, "localPlayback": true,
	"enqueueDownloads": true, "downloads": true, "controlDownloads": true, "controlDownloadBatch": true,
	"downloadDirectory": true, "storage": true, "sourceStatus": true, "sourceJob": true,
	"cancelSourceJob": true, "workLease": true, "resourceSettings": true, "saveResourceSettings": true,
}

func (drama dramaInput) payload() map[string]any {
	if drama.MetadataSchema == 0 {
		drama.MetadataSchema = 1
	}
	return map[string]any{"drama": drama}
}

func limitString(value string, size int) string {
	value = strings.TrimSpace(value)
	if len(value) > size {
		return value[:size]
	}
	return value
}

func (chapter chapterInput) payload() map[string]any {
	body := string(chapter.CurrentEpisode)
	if trimmed := strings.TrimSpace(body); trimmed == "" ||
		(!strings.HasPrefix(trimmed, `"`) && !isNumericPrefix(trimmed)) {
		chapter.CurrentEpisode = nil
	}
	chapter.ID = limitString(chapter.ID, 240)
	chapter.Source = limitString(chapter.Source, 60)
	chapter.Title = limitString(chapter.Title, 240)
	if chapter.MediaSize < 0 || chapter.MediaSize > 1<<40 {
		chapter.MediaSize = 0
	}
	return map[string]any{"id": chapter.ID, "source": chapter.Source, "title": chapter.Title,
		"currentEpisode": chapter.CurrentEpisode, "mediaSize": chapter.MediaSize}
}

func isNumericPrefix(value string) bool {
	return value != "" && value[0] >= '0' && value[0] <= '9'
}

func (request clientRequest) fields() (map[string]any, error) {
	fields := map[string]any{}
	switch request.Action {
	case "catalog", "cached", "categories", "suggestions", "recommendations", "cachedRecommendations", "sourceStatus", "sourceJob", "cancelSourceJob":
		fields["source"] = limitString(request.Source, 60)
		if request.Category != "" {
			fields["category"] = limitString(request.Category, 80)
		}
		if request.Query != "" {
			fields["query"] = limitString(request.Query, 120)
		}
		if request.Page > 0 {
			fields["page"] = min(request.Page, 500)
		}
		if request.Force {
			fields["force"] = true
		}
		if request.Drama.ID != "" {
			fields["drama"] = request.Drama.payload()["drama"]
		}
		if request.Command != "" {
			fields["command"] = limitString(request.Command, 40)
		}
	case "rankings":
		fields["board"] = limitString(request.Board, 60)
		if request.Force {
			fields["force"] = true
		}
	case "detail", "metadata", "cover", "prepareCover":
		if request.Drama.ID == "" {
			return nil, errors.New("缺少剧集信息")
		}
		fields["drama"] = request.Drama.payload()["drama"]
		if request.Force {
			fields["force"] = true
		}
	case "resolve":
		if request.Drama.ID == "" {
			return nil, errors.New("缺少剧集信息")
		}
		if len(request.Chapters) != 1 {
			return nil, errors.New("缺少分集信息")
		}
		fields["drama"] = request.Drama.payload()["drama"]
		fields["chapter"] = request.Chapters[0].payload()
		fields["index"] = request.Index
		fields["quality"] = request.Quality
	case "localPlayback":
		if request.Drama.ID == "" {
			return nil, errors.New("缺少剧集信息")
		}
		fields["drama"] = request.Drama.payload()["drama"]
		fields["index"] = request.Index
	case "enqueueDownloads":
		if request.Drama.ID == "" {
			return nil, errors.New("缺少剧集信息")
		}
		if len(request.Entries) == 0 || len(request.Entries) > 500 {
			return nil, errors.New("请选择 1 至 500 集加入下载")
		}
		entries := make([]map[string]any, 0, len(request.Entries))
		for _, entry := range request.Entries {
			if entry.Index < 1 || entry.Index > 100000 {
				return nil, errors.New("下载集数无效")
			}
			entries = append(entries, map[string]any{"chapter": entry.Chapter.payload(), "index": entry.Index})
		}
		fields["drama"] = request.Drama.payload()["drama"]
		fields["entries"] = entries
		fields["quality"] = request.Quality
		if request.Force {
			fields["force"] = true
		}
	case "controlDownloads", "workLease":
		fields["jobId"] = limitString(request.JobID, 80)
		fields["command"] = limitString(request.Command, 40)
	case "controlDownloadBatch":
		if len(request.JobIDs) == 0 || len(request.JobIDs) > 1000 {
			return nil, errors.New("请选择要操作的任务")
		}
		ids := make([]string, 0, len(request.JobIDs))
		for _, id := range request.JobIDs {
			if !isHexID(id) {
				return nil, errors.New("任务编号无效")
			}
			ids = append(ids, id)
		}
		fields["jobIds"] = ids
		fields["command"] = limitString(request.Command, 40)
		if len(request.ExpectedVersions) > 0 {
			versions := map[string]string{}
			for id, version := range request.ExpectedVersions {
				if isHexID(id) && len(versions) < len(ids) {
					versions[id] = limitString(version, 80)
				}
			}
			fields["expectedVersions"] = versions
		}
	case "downloads", "downloadDirectory", "storage", "resourceSettings":
	case "saveResourceSettings":
		settings := request.Settings
		settings.ProxyMode = limitString(settings.ProxyMode, 20)
		if settings.ProxyMode != "auto" && settings.ProxyMode != "direct" && settings.ProxyMode != "manual" {
			return nil, errors.New("代理模式无效")
		}
		settings.ProxyURL = limitString(strings.TrimSpace(settings.ProxyURL), 2048)
		if settings.ProxyMode == "manual" && settings.ProxyURL == "" {
			return nil, errors.New("请填写代理地址")
		}
		fields["settings"] = settings
	default:
		return nil, errors.New("不支持的操作")
	}
	return fields, nil
}

func isHexID(value string) bool {
	if len(value) != 32 {
		return false
	}
	for _, character := range value {
		if !(character >= '0' && character <= '9' || character >= 'a' && character <= 'f') {
			return false
		}
	}
	return true
}

func (app *server) rpc(writer http.ResponseWriter, request *http.Request) {
	var input clientRequest
	if err := json.NewDecoder(io.LimitReader(request.Body, 1<<20)).Decode(&input); err != nil {
		writeJSON(writer, http.StatusBadRequest, map[string]any{"ok": false, "error": "请求格式无效"})
		return
	}
	if !rpcActions[input.Action] {
		writeJSON(writer, http.StatusBadRequest, map[string]any{"ok": false, "error": "不支持的操作"})
		return
	}
	fields, err := input.fields()
	if err == nil {
		_, err = app.downloadRoot()
	}
	if err == nil {
		var data json.RawMessage
		data, err = coreCall(input.Action, fields)
		if err == nil {
			writeJSON(writer, http.StatusOK, map[string]any{"ok": true, "data": data})
			return
		}
	}
	app.failure(writer, err)
}

func sanitizeError(message string) string {
	if index := strings.IndexAny(message, "\r\n"); index >= 0 {
		message = message[:index]
	}
	message = strings.TrimSpace(message)
	if len(message) > 300 {
		message = message[:300]
	}
	if message == "" {
		return "站源请求失败，请稍后重试"
	}
	return message
}

func (app *server) failure(writer http.ResponseWriter, err error) {
	status := http.StatusBadGateway
	body := map[string]any{"ok": false, "error": sanitizeError(err.Error())}
	var failure *coreError
	if errors.As(err, &failure) {
		body["code"] = failure.code
		if !failure.retryAt.IsZero() {
			body["retryAt"] = failure.retryAt
		}
		if failure.code == "local_media" {
			status = http.StatusNotFound
		}
	}
	writeJSON(writer, status, body)
}

func (app *server) downloadRoot() (string, error) {
	data, err := coreCall("downloadDirectory", nil)
	if err != nil {
		return "", err
	}
	var value struct {
		Directory string `json:"directory"`
	}
	if json.Unmarshal(data, &value) != nil || !filepath.IsAbs(value.Directory) {
		return "", errors.New("无法确认下载目录")
	}
	return filepath.Clean(value.Directory), nil
}

func (app *server) insideRoot(candidate string) (string, error) {
	root, err := app.downloadRoot()
	if err != nil {
		return "", err
	}
	resolved, err := filepath.EvalSymlinks(candidate)
	if err != nil {
		return "", err
	}
	relation, err := filepath.Rel(root, resolved)
	if err != nil || relation == ".." || strings.HasPrefix(relation, ".."+string(filepath.Separator)) ||
		filepath.IsAbs(relation) {
		return "", errors.New("文件路径不在下载目录内")
	}
	info, err := os.Stat(resolved)
	if err != nil || !info.Mode().IsRegular() {
		return "", errors.New("文件不存在")
	}
	return resolved, nil
}

type snapshotJob struct {
	ID            string  `json:"id"`
	State         string  `json:"state"`
	Archived      bool    `json:"archived"`
	Folder        string  `json:"folder"`
	File          string  `json:"file"`
	Bytes         int64   `json:"bytes"`
	Total         int64   `json:"total"`
	Progress      float64 `json:"progress"`
	Index         int     `json:"index"`
	Quality       int     `json:"quality"`
	ActualQuality int     `json:"actualQuality"`
	Created       int64   `json:"created"`
	Revision      int64   `json:"revision"`
	Error         string  `json:"error"`
	Chapter       struct {
		ID             string          `json:"id"`
		Title          string          `json:"title"`
		Source         string          `json:"source"`
		CurrentEpisode json.RawMessage `json:"currentEpisode"`
	} `json:"chapter"`
	Drama dramaInput `json:"drama"`
}

func (job snapshotJob) version() string {
	version := strconv.FormatInt(job.Created, 10) + "-" + strconv.FormatInt(job.Bytes, 10) +
		"-" + strconv.Itoa(job.ActualQuality)
	if job.Revision > 0 {
		version += "-" + strconv.FormatInt(job.Revision, 10)
	}
	return version
}

func (app *server) jobs() ([]snapshotJob, error) {
	data, err := coreCall("downloads", nil)
	if err != nil {
		return nil, err
	}
	var value struct {
		Jobs []snapshotJob `json:"jobs"`
	}
	if json.Unmarshal(data, &value) != nil {
		return nil, fmt.Errorf("下载记录无法解析")
	}
	return value.Jobs, nil
}

func (app *server) coverPath(drama dramaInput) (string, error) {
	data, err := coreCall("cover", drama.payload())
	if err != nil {
		return "", err
	}
	var value struct {
		Path string `json:"path"`
	}
	if json.Unmarshal(data, &value) != nil || !filepath.IsAbs(value.Path) {
		return "", errors.New("封面加载失败")
	}
	return value.Path, nil
}

func (app *server) cover(writer http.ResponseWriter, request *http.Request) {
	query := request.URL.Query()
	drama := dramaInput{ID: limitString(query.Get("id"), 240), Source: limitString(query.Get("source"), 60),
		Cover: strings.TrimSpace(query.Get("cover"))}
	if drama.ID == "" || !app.sourceAllowed(dramaSource(drama)) {
		http.Error(writer, "剧集信息无效", http.StatusBadRequest)
		return
	}
	if err := app.checkCoverURL(drama.Cover); err != nil {
		http.Error(writer, err.Error(), http.StatusBadRequest)
		return
	}
	path, err := app.coverPath(drama)
	if err != nil {
		http.Error(writer, "封面加载失败", http.StatusBadGateway)
		return
	}
	switch strings.ToLower(filepath.Ext(path)) {
	case ".jpg", ".jpeg":
		writer.Header().Set("Content-Type", "image/jpeg")
	case ".png":
		writer.Header().Set("Content-Type", "image/png")
	case ".gif":
		writer.Header().Set("Content-Type", "image/gif")
	default:
		writer.Header().Set("Content-Type", "image/webp")
	}
	writer.Header().Set("Cache-Control", "private, max-age=86400")
	http.ServeFile(writer, request, path)
}

func dramaSource(drama dramaInput) string {
	if value := strings.TrimSpace(drama.Source); value != "" {
		return value
	}
	if index := strings.Index(drama.ID, ":"); index > 0 {
		return drama.ID[:index]
	}
	return ""
}

func (app *server) sourceAllowed(source string) bool {
	available, _ := app.availableSources()
	for _, candidate := range available {
		if candidate == source {
			return true
		}
	}
	return false
}

func (app *server) availableSources() ([]string, bool) {
	all := false
	if value, ok := app.info["allSources"].(bool); ok {
		all = value
	}
	if all {
		return []string{"hongguo", "huangdou", "huangju", "huangguo-video", "huangguoai", "cloudfront"}, all
	}
	return []string{"hongguo"}, all
}

func (app *server) checkCoverURL(raw string) error {
	address, err := url.Parse(raw)
	if err != nil || address.Scheme != "https" || address.Hostname() == "" || address.User != nil {
		return errors.New("封面地址无效")
	}
	records, err := net.LookupIP(address.Hostname())
	if err != nil || len(records) == 0 {
		return errors.New("封面域名无法解析")
	}
	for _, record := range records {
		if record.IsLoopback() || record.IsPrivate() || record.IsLinkLocalUnicast() ||
			record.IsLinkLocalMulticast() || record.IsMulticast() || record.IsUnspecified() {
			return errors.New("封面地址不在公网范围")
		}
	}
	return nil
}

func (app *server) file(writer http.ResponseWriter, request *http.Request) {
	query := request.URL.Query()
	path, err := app.filePath(query.Get("job"), query.Get("id"))
	if err != nil {
		http.Error(writer, err.Error(), http.StatusNotFound)
		return
	}
	name := filepath.Base(path)
	if value := query.Get("name"); value != "" {
		name = filepath.Base(value)
	}
	serveFile(writer, request, path, query.Get("download") == "1", name)
}

func (app *server) filePath(job string, id string) (string, error) {
	switch {
	case id != "":
		if !isHexID(id) {
			return "", errors.New("文件编号无效")
		}
		if item := app.merges.library.item(id); item != nil {
			return app.insideRoot(filepath.Join(app.merges.root, item.File))
		}
		return "", errors.New("文件不存在")
	case job != "":
		if !isHexID(job) {
			return "", errors.New("任务编号无效")
		}
		records, err := app.jobs()
		if err != nil {
			return "", err
		}
		for _, record := range records {
			if record.ID != job || record.State != "completed" || record.File == "" {
				continue
			}
			directory := filepath.Join(app.merges.root, record.ID)
			if record.Folder != "" {
				directory = filepath.Join(app.merges.root, filepath.FromSlash(record.Folder))
			}
			return app.insideRoot(filepath.Join(directory, filepath.Base(record.File)))
		}
		return "", errors.New("文件尚未下载完成")
	default:
		return "", errors.New("缺少文件编号")
	}
}
