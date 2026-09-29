package main

import (
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"log"
	"net"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"
)

const sessionCookie = "nas_session"
const sessionLifetime = 30 * 24 * time.Hour

type loginFailure struct {
	count int
	until time.Time
}

type sessionStore struct {
	mu       sync.Mutex
	sessions map[string]time.Time
	fails    map[string]loginFailure
	password []byte
}

func newSessions(password string) *sessionStore {
	return &sessionStore{
		sessions: map[string]time.Time{},
		fails:    map[string]loginFailure{},
		password: []byte(password),
	}
}

func randomToken() (string, error) {
	value := make([]byte, 32)
	if _, err := rand.Read(value); err != nil {
		return "", err
	}
	return hex.EncodeToString(value), nil
}

func (store *sessionStore) login(address, password string) (string, error) {
	store.mu.Lock()
	defer store.mu.Unlock()
	fail := store.fails[address]
	if fail.count >= 6 && time.Now().Before(fail.until) {
		return "", errors.New("尝试次数过多，请 " + strconv.Itoa(int(time.Until(fail.until).Seconds())+1) + " 秒后再试")
	}
	if subtle.ConstantTimeCompare([]byte(password), store.password) != 1 {
		fail.count++
		fail.until = time.Now().Add(time.Duration(min(fail.count, 6)) * 15 * time.Second)
		store.fails[address] = fail
		return "", errors.New("访问密码不正确")
	}
	token, err := randomToken()
	if err != nil {
		return "", err
	}
	delete(store.fails, address)
	store.sessions[token] = time.Now().Add(sessionLifetime)
	for candidate, until := range store.sessions {
		if time.Now().After(until) {
			delete(store.sessions, candidate)
		}
	}
	return token, nil
}

func (store *sessionStore) valid(token string) bool {
	if len(token) != 64 {
		return false
	}
	store.mu.Lock()
	defer store.mu.Unlock()
	for candidate, until := range store.sessions {
		if time.Now().After(until) {
			delete(store.sessions, candidate)
			continue
		}
		if subtle.ConstantTimeCompare([]byte(candidate), []byte(token)) == 1 {
			return true
		}
	}
	return false
}

func (store *sessionStore) drop(token string) {
	store.mu.Lock()
	delete(store.sessions, token)
	store.mu.Unlock()
}

func requestAddress(request *http.Request) string {
	host, _, err := net.SplitHostPort(strings.Trim(request.RemoteAddr, "[]"))
	if err != nil {
		return request.RemoteAddr
	}
	return host
}

type server struct {
	settings settings
	sessions *sessionStore
	merges   *mergeManager
	info     map[string]any
}

func (app *server) protected(next http.Handler) http.Handler {
	return http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		switch request.URL.Path {
		case "/login", "/api/login", "/assets/app.css":
			next.ServeHTTP(writer, request)
			return
		}
		cookie, err := request.Cookie(sessionCookie)
		if err != nil || !app.sessions.valid(cookie.Value) {
			if strings.HasPrefix(request.URL.Path, "/api/") {
				writeJSON(writer, http.StatusUnauthorized, map[string]any{"ok": false, "error": "请先输入访问密码"})
				return
			}
			http.Redirect(writer, request, "/login", http.StatusSeeOther)
			return
		}
		if request.Method != http.MethodGet && request.Header.Get("X-Requested-With") != "guoapp-nas" {
			writeJSON(writer, http.StatusForbidden, map[string]any{"ok": false, "error": "请求来源校验未通过"})
			return
		}
		next.ServeHTTP(writer, request)
	})
}

func writeJSON(writer http.ResponseWriter, status int, body any) {
	writer.Header().Set("Content-Type", "application/json; charset=utf-8")
	writer.Header().Set("Cache-Control", "no-store")
	writer.WriteHeader(status)
	if err := json.NewEncoder(writer).Encode(body); err != nil {
		log.Println("响应写入失败", err)
	}
}

func (app *server) login(writer http.ResponseWriter, request *http.Request) {
	if request.Method == http.MethodGet {
		app.asset(writer, request, "web/login.html")
		return
	}
	var input struct {
		Password string `json:"password"`
	}
	if err := json.NewDecoder(io.LimitReader(request.Body, 4096)).Decode(&input); err != nil {
		writeJSON(writer, http.StatusBadRequest, map[string]any{"ok": false, "error": "请求格式无效"})
		return
	}
	token, err := app.sessions.login(requestAddress(request), input.Password)
	if err != nil {
		writeJSON(writer, http.StatusForbidden, map[string]any{"ok": false, "error": err.Error()})
		return
	}
	http.SetCookie(writer, &http.Cookie{
		Name: sessionCookie, Value: token, Path: "/", HttpOnly: true,
		SameSite: http.SameSiteLaxMode, MaxAge: int(sessionLifetime.Seconds()),
	})
	writeJSON(writer, http.StatusOK, map[string]any{"ok": true})
}

func (app *server) logout(writer http.ResponseWriter, request *http.Request) {
	if cookie, err := request.Cookie(sessionCookie); err == nil {
		app.sessions.drop(cookie.Value)
	}
	http.SetCookie(writer, &http.Cookie{Name: sessionCookie, Value: "", Path: "/", MaxAge: -1, HttpOnly: true})
	writeJSON(writer, http.StatusOK, map[string]any{"ok": true})
}

func (app *server) sessionInfo(writer http.ResponseWriter, request *http.Request) {
	writeJSON(writer, http.StatusOK, map[string]any{"ok": true, "data": app.info})
}
