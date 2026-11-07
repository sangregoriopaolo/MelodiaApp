// Package api exposes the HTTP endpoints used by the Melodia Android app.
// The contract is documented in docs/sync-api.md at the repository root.
package api

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net"
	"net/http"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/sangregoriopaolo/melodiaapp/server/internal/store"
)

const (
	maxBodyBytes     = 4 << 20 // 4 MB: thousands of songs with metadata
	maxItems         = 10000
	maxNameLen       = 200
	maxIDLen         = 64
	maxFieldLen      = 2048
	loginMaxFailures = 10
	loginWindow      = 15 * time.Minute
)

type Server struct {
	store      *store.Store
	limiter    *limiter
	trustProxy bool
	log        *slog.Logger
}

// New builds the HTTP handler. trustProxy makes the login rate limiter use
// X-Forwarded-For / X-Real-IP, which is right when running behind a reverse proxy.
func New(st *store.Store, trustProxy bool, log *slog.Logger) http.Handler {
	s := &Server{store: st, limiter: newLimiter(loginMaxFailures, loginWindow), trustProxy: trustProxy, log: log}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) { w.Write([]byte("ok")) })
	mux.HandleFunc("POST /api/v1/login", s.login)
	mux.HandleFunc("POST /api/v1/logout", s.auth(s.logout))
	mux.HandleFunc("GET /api/v1/me", s.auth(s.me))
	mux.HandleFunc("GET /api/v1/sync", s.auth(s.sync))
	mux.HandleFunc("PUT /api/v1/playlists/{id}", s.auth(s.putPlaylist))
	mux.HandleFunc("DELETE /api/v1/playlists/{id}", s.auth(s.deletePlaylist))
	return mux
}

type ctxKey struct{}

type authed struct {
	store.Session
	token string
}

func (s *Server) auth(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		token, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
		if !ok || token == "" {
			writeErr(w, http.StatusUnauthorized, "missing bearer token")
			return
		}
		sess, err := s.store.Authenticate(r.Context(), token)
		if errors.Is(err, store.ErrNotFound) {
			writeErr(w, http.StatusUnauthorized, "invalid or expired token")
			return
		}
		if err != nil {
			s.internal(w, err)
			return
		}
		next(w, r.WithContext(context.WithValue(r.Context(), ctxKey{}, authed{sess, token})))
	}
}

func session(r *http.Request) authed { return r.Context().Value(ctxKey{}).(authed) }

func (s *Server) clientIP(r *http.Request) string {
	if s.trustProxy {
		if ip := strings.TrimSpace(r.Header.Get("X-Real-IP")); ip != "" {
			return ip
		}
		if xff := r.Header.Get("X-Forwarded-For"); xff != "" {
			parts := strings.Split(xff, ",")
			return strings.TrimSpace(parts[len(parts)-1])
		}
	}
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}

type loginReq struct {
	Username   string `json:"username"`
	Password   string `json:"password"`
	DeviceName string `json:"deviceName"`
}

func (s *Server) login(w http.ResponseWriter, r *http.Request) {
	var req loginReq
	if !decode(w, r, &req) {
		return
	}
	ipKey := "ip:" + s.clientIP(r)
	userKey := "user:" + strings.ToLower(strings.TrimSpace(req.Username))
	if s.limiter.blocked(ipKey) || s.limiter.blocked(userKey) {
		writeErr(w, http.StatusTooManyRequests, "too many failed attempts, try again later")
		return
	}
	token, err := s.store.Login(r.Context(), req.Username, req.Password, truncate(req.DeviceName, maxNameLen))
	if errors.Is(err, store.ErrBadCredential) {
		s.limiter.fail(ipKey)
		s.limiter.fail(userKey)
		writeErr(w, http.StatusUnauthorized, "invalid username or password")
		return
	}
	if err != nil {
		s.internal(w, err)
		return
	}
	s.limiter.reset(userKey)
	writeJSON(w, http.StatusOK, map[string]string{"token": token, "username": strings.TrimSpace(req.Username)})
}

func (s *Server) logout(w http.ResponseWriter, r *http.Request) {
	if err := s.store.Logout(r.Context(), session(r).token); err != nil {
		s.internal(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) me(w http.ResponseWriter, r *http.Request) {
	sess := session(r)
	writeJSON(w, http.StatusOK, map[string]string{"username": sess.Username, "deviceName": sess.DeviceName})
}

func (s *Server) sync(w http.ResponseWriter, r *http.Request) {
	since := int64(0)
	if v := r.URL.Query().Get("since"); v != "" {
		n, err := strconv.ParseInt(v, 10, 64)
		if err != nil || n < 0 {
			writeErr(w, http.StatusBadRequest, "since must be a non-negative integer")
			return
		}
		since = n
	}
	playlists, rev, err := s.store.Changes(r.Context(), session(r).UserID, since)
	if err != nil {
		s.internal(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"rev": rev, "playlists": playlists})
}

type putReq struct {
	BaseRev   int64        `json:"baseRev"`
	Name      string       `json:"name"`
	UpdatedAt int64        `json:"updatedAt"`
	Items     []store.Item `json:"items"`
}

func (s *Server) putPlaylist(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if !validID(id) {
		writeErr(w, http.StatusBadRequest, "invalid playlist id")
		return
	}
	var req putReq
	if !decode(w, r, &req) {
		return
	}
	if msg := validatePut(&req); msg != "" {
		writeErr(w, http.StatusBadRequest, msg)
		return
	}
	if req.Items == nil {
		req.Items = []store.Item{}
	}
	p := store.Playlist{ID: id, Name: req.Name, UpdatedAt: req.UpdatedAt, Items: req.Items}
	rev, err := s.store.Put(r.Context(), session(r).UserID, p, req.BaseRev)
	s.writeResult(w, rev, err)
}

func (s *Server) deletePlaylist(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if !validID(id) {
		writeErr(w, http.StatusBadRequest, "invalid playlist id")
		return
	}
	q := r.URL.Query()
	baseRev, err1 := strconv.ParseInt(q.Get("baseRev"), 10, 64)
	updatedAt, err2 := strconv.ParseInt(q.Get("updatedAt"), 10, 64)
	if err1 != nil || err2 != nil || baseRev < 0 || updatedAt <= 0 {
		writeErr(w, http.StatusBadRequest, "baseRev and updatedAt query parameters are required")
		return
	}
	rev, err := s.store.Delete(r.Context(), session(r).UserID, id, baseRev, updatedAt)
	s.writeResult(w, rev, err)
}

func (s *Server) writeResult(w http.ResponseWriter, rev int64, err error) {
	var conflict *store.ConflictError
	switch {
	case errors.As(err, &conflict):
		writeJSON(w, http.StatusConflict, map[string]any{"error": "conflict", "current": conflict.Current})
	case err != nil:
		s.internal(w, err)
	default:
		writeJSON(w, http.StatusOK, map[string]int64{"rev": rev})
	}
}

func validID(id string) bool {
	if id == "" || len(id) > maxIDLen {
		return false
	}
	for _, c := range id {
		if !(c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '_' || c == '-') {
			return false
		}
	}
	return true
}

func validatePut(req *putReq) string {
	req.Name = strings.TrimSpace(req.Name)
	switch {
	case req.BaseRev < 0:
		return "baseRev must be >= 0"
	case req.Name == "" || utf8.RuneCountInString(req.Name) > maxNameLen:
		return "name is required (max 200 characters)"
	case req.UpdatedAt <= 0:
		return "updatedAt (unix milliseconds) is required"
	case len(req.Items) > maxItems:
		return "too many items"
	}
	for _, it := range req.Items {
		if it.SongID == "" || len(it.SongID) > maxIDLen {
			return "every item needs a songId"
		}
		for _, f := range []string{it.SetVideoID, it.Title, it.Artists, it.Album, it.ThumbnailURL} {
			if len(f) > maxFieldLen {
				return "item field too long"
			}
		}
	}
	return ""
}

func truncate(s string, n int) string {
	if utf8.RuneCountInString(s) <= n {
		return s
	}
	return string([]rune(s)[:n])
}

func decode(w http.ResponseWriter, r *http.Request, v any) bool {
	r.Body = http.MaxBytesReader(w, r.Body, maxBodyBytes)
	if err := json.NewDecoder(r.Body).Decode(v); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid JSON body")
		return false
	}
	return true
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(v)
}

func writeErr(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]string{"error": msg})
}

func (s *Server) internal(w http.ResponseWriter, err error) {
	s.log.Error("request failed", "err", err)
	writeErr(w, http.StatusInternalServerError, "internal error")
}
