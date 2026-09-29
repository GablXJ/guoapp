package main

import (
	"context"
	"embed"
	"errors"
	"flag"
	"log"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"
)

//go:embed web
var assets embed.FS

type settings struct {
	Address     string
	Directory   string
	DownloadDir string
	Password    string
	FFmpeg      string
	FFprobe     string
}

func loadSettings() (settings, error) {
	value := settings{
		Address:     firstNonEmpty(os.Getenv("NAS_ADDR"), ":8090"),
		Directory:   firstNonEmpty(os.Getenv("NAS_DATA"), "/data"),
		DownloadDir: os.Getenv("NAS_DOWNLOAD_DIR"),
		Password:    os.Getenv("NAS_PASSWORD"),
		FFmpeg:      firstNonEmpty(os.Getenv("NAS_FFMPEG"), "ffmpeg"),
		FFprobe:     firstNonEmpty(os.Getenv("NAS_FFPROBE"), "ffprobe"),
	}
	if strings.HasPrefix(value.Address, ":") && len(value.Address) > 1 {
		if _, err := strconv.Atoi(value.Address[1:]); err != nil {
			return value, errors.New("NAS_ADDR 端口无效")
		}
	} else if !strings.Contains(value.Address, ":") {
		return value, errors.New("NAS_ADDR 需形如 :8090 或 127.0.0.1:8090")
	}
	absolute, err := filepath.Abs(value.Directory)
	if err != nil {
		return value, err
	}
	value.Directory = absolute
	if value.Password == "" && flag.Lookup("test.v") == nil {
		return value, errors.New("必须设置 NAS_PASSWORD 访问密码")
	}
	return value, nil
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			return strings.TrimSpace(value)
		}
	}
	return ""
}

func (app *server) asset(writer http.ResponseWriter, request *http.Request, name string) {
	file := strings.TrimPrefix(name, "web/")
	if file == "" || strings.Contains(file, "..") {
		http.NotFound(writer, request)
		return
	}
	body, err := assets.ReadFile("web/" + file)
	if err != nil {
		http.NotFound(writer, request)
		return
	}
	switch filepath.Ext(file) {
	case ".html":
		writer.Header().Set("Content-Type", "text/html; charset=utf-8")
	case ".css":
		writer.Header().Set("Content-Type", "text/css; charset=utf-8")
	case ".js":
		writer.Header().Set("Content-Type", "text/javascript; charset=utf-8")
	}
	writer.Header().Set("Cache-Control", "no-cache")
	_, _ = writer.Write(body)
}

func (app *server) assetRoute(writer http.ResponseWriter, request *http.Request) {
	app.asset(writer, request, "web/"+strings.TrimPrefix(request.URL.Path, "/assets/"))
}

func (app *server) index(writer http.ResponseWriter, request *http.Request) {
	if request.URL.Path != "/" {
		http.NotFound(writer, request)
		return
	}
	app.asset(writer, request, "web/index.html")
}

func (app *server) routes() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /{$}", app.index)
	mux.HandleFunc("GET /login", app.login)
	mux.HandleFunc("POST /api/login", app.login)
	mux.HandleFunc("POST /api/logout", app.logout)
	mux.HandleFunc("GET /api/session", app.sessionInfo)
	mux.HandleFunc("GET /api/state", app.state)
	mux.HandleFunc("POST /api/rpc", app.rpc)
	mux.HandleFunc("POST /api/merge", app.mergeStart)
	mux.HandleFunc("POST /api/merge/control", app.mergeControl)
	mux.HandleFunc("GET /api/merge/list", app.mergeList)
	mux.HandleFunc("GET /api/library/list", app.libraryList)
	mux.HandleFunc("POST /api/library/export", app.libraryExport)
	mux.HandleFunc("POST /api/library/delete", app.libraryDelete)
	mux.HandleFunc("GET /cover", app.cover)
	mux.HandleFunc("GET /file", app.file)
	mux.HandleFunc("GET /assets/", app.assetRoute)
	return app.protected(mux)
}

func main() {
	log.SetFlags(log.Ltime)
	config, err := loadSettings()
	if err != nil {
		log.Fatal("启动失败：", err)
	}
	if err := os.MkdirAll(config.Directory, 0700); err != nil {
		log.Fatal("启动失败：无法创建数据目录 ", config.Directory)
	}
	info, err := coreInitialize(config.Directory)
	if err != nil {
		log.Fatal("启动失败：本地核心未就绪 ", err)
	}
	app := &server{settings: config, info: info}
	root, err := app.downloadRoot()
	if err != nil {
		log.Fatal("启动失败：本地核心未就绪 ", err)
	}
	if config.DownloadDir != "" {
		parent, prepareErr := filepath.Abs(config.DownloadDir)
		if prepareErr != nil {
			log.Println("下载目录配置无效，已忽略：", prepareErr)
		} else if err := os.MkdirAll(parent, 0700); err != nil {
			log.Println("无法创建 NAS_DOWNLOAD_DIR，已忽略：", err)
		} else if !strings.EqualFold(filepath.Clean(parent), filepath.Clean(filepath.Dir(root))) {
			if _, moveErr := coreCall("moveDownloads", map[string]any{"directory": parent}); moveErr != nil {
				log.Println("下载目录迁移失败：", moveErr)
			} else if root, err = app.downloadRoot(); err != nil {
				log.Println("下载目录读取失败：", err)
			} else {
				log.Println("下载目录已迁移至", root)
			}
		}
	}
	if app.merges, err = newMergeManager(app, root); err != nil {
		log.Fatal("启动失败：合并模块未就绪 ", err)
	}
	app.sessions = newSessions(config.Password)
	server := &http.Server{
		Addr: config.Address, Handler: app.routes(),
		ReadHeaderTimeout: 5 * time.Second, ReadTimeout: 60 * time.Second,
		IdleTimeout: 90 * time.Second, MaxHeaderBytes: 16384,
	}
	go app.merges.serve(context.Background())
	go func() {
		log.Println("NAS 版已启动，监听", config.Address)
		if err := server.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Fatal("监听失败：", err)
		}
	}()
	shutdown := make(chan os.Signal, 1)
	signal.Notify(shutdown, os.Interrupt, syscall.SIGTERM)
	<-shutdown
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	_ = server.Shutdown(ctx)
	app.merges.stop()
}

func serveFile(writer http.ResponseWriter, request *http.Request, path string, download bool, name string) {
	file, err := os.Open(path)
	if err != nil {
		http.Error(writer, "文件不存在", http.StatusNotFound)
		return
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil || !info.Mode().IsRegular() {
		http.Error(writer, "文件不存在", http.StatusNotFound)
		return
	}
	if download {
		writer.Header().Set("Content-Disposition", "attachment; filename*=UTF-8''"+url.PathEscape(name))
	}
	http.ServeContent(writer, request, filepath.Base(path), info.ModTime(), file)
}
