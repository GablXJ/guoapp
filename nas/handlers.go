package main

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
)

func (app *server) mergeStart(writer http.ResponseWriter, request *http.Request) {
	var input struct {
		JobIDs []string `json:"jobIds"`
	}
	if err := json.NewDecoder(io.LimitReader(request.Body, 1<<20)).Decode(&input); err != nil {
		writeJSON(writer, http.StatusBadRequest, map[string]any{"ok": false, "error": "请求格式无效"})
		return
	}
	if len(input.JobIDs) < 2 || len(input.JobIDs) > 10000 {
		writeJSON(writer, http.StatusBadRequest, map[string]any{"ok": false, "error": "请选择 2 至 10000 集已下载视频"})
		return
	}
	for _, id := range input.JobIDs {
		if !isHexID(id) {
			writeJSON(writer, http.StatusBadRequest, map[string]any{"ok": false, "error": "任务编号无效"})
			return
		}
	}
	job, err := app.merges.start(input.JobIDs)
	if err != nil {
		app.failure(writer, err)
		return
	}
	writeJSON(writer, http.StatusOK, map[string]any{"ok": true, "data": job})
}

func (app *server) mergeControl(writer http.ResponseWriter, request *http.Request) {
	var input struct {
		ID      string `json:"id"`
		Command string `json:"command"`
	}
	if err := json.NewDecoder(io.LimitReader(request.Body, 4096)).Decode(&input); err != nil || !isHexID(input.ID) {
		writeJSON(writer, http.StatusBadRequest, map[string]any{"ok": false, "error": "请求格式无效"})
		return
	}
	if input.Command != "cancel" && input.Command != "retry" && input.Command != "remove" {
		writeJSON(writer, http.StatusBadRequest, map[string]any{"ok": false, "error": "不支持的合并操作"})
		return
	}
	if err := app.merges.control(input.ID, input.Command); err != nil {
		app.failure(writer, err)
		return
	}
	writeJSON(writer, http.StatusOK, map[string]any{"ok": true})
}

func (app *server) mergeList(writer http.ResponseWriter, request *http.Request) {
	writeJSON(writer, http.StatusOK, map[string]any{"ok": true,
		"data": map[string]any{"jobs": app.merges.snapshot(), "items": app.merges.library.list()}})
}

func (app *server) libraryList(writer http.ResponseWriter, request *http.Request) {
	writeJSON(writer, http.StatusOK, map[string]any{"ok": true, "data": app.merges.library.list()})
}

func (app *server) libraryExport(writer http.ResponseWriter, request *http.Request) {
	var input struct {
		ID string `json:"id"`
	}
	if err := json.NewDecoder(io.LimitReader(request.Body, 4096)).Decode(&input); err != nil || !isHexID(input.ID) {
		writeJSON(writer, http.StatusBadRequest, map[string]any{"ok": false, "error": "请求格式无效"})
		return
	}
	relative, err := app.merges.export(input.ID)
	if err != nil {
		app.failure(writer, err)
		return
	}
	writeJSON(writer, http.StatusOK, map[string]any{"ok": true, "data": map[string]string{"folder": relative}})
}

func (app *server) libraryDelete(writer http.ResponseWriter, request *http.Request) {
	var input struct {
		ID   string `json:"id"`
		File bool   `json:"file"`
	}
	if err := json.NewDecoder(io.LimitReader(request.Body, 4096)).Decode(&input); err != nil || !isHexID(input.ID) {
		writeJSON(writer, http.StatusBadRequest, map[string]any{"ok": false, "error": "请求格式无效"})
		return
	}
	if err := app.merges.deleteItem(input.ID, input.File); err != nil {
		app.failure(writer, err)
		return
	}
	writeJSON(writer, http.StatusOK, map[string]any{"ok": true})
}

func (app *server) state(writer http.ResponseWriter, request *http.Request) {
	root, err := app.downloadRoot()
	if err != nil {
		app.failure(writer, errors.New("无法确认下载目录"))
		return
	}
	sources, all := app.availableSources()
	writeJSON(writer, http.StatusOK, map[string]any{"ok": true, "data": map[string]any{
		"version": app.info["version"], "allSources": all, "sources": sources,
		"directory": root, "mergeRunning": app.merges.running(),
	}})
}
