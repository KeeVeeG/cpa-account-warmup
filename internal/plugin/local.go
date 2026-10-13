package plugin

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"io"
	"net"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	sessionLifetime = 30 * time.Minute
	maxSessions     = 64
	maxRequestBody  = 8192
)

type localServer struct {
	plugin        *Server
	http          *http.Server
	address       string
	origin        string
	launchPath    string
	bootstrapHash [32]byte
	done          chan struct{}
	mu            sync.Mutex
	closed        bool
	sessions      map[[32]byte]time.Time
	now           func() time.Time
}

func (s *Server) startLocal() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return http.ErrServerClosed
	}
	if s.local != nil {
		return nil
	}
	capability, err := randomToken()
	if err != nil {
		return err
	}
	listener, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		return err
	}
	local := &localServer{
		plugin: s, address: listener.Addr().String(),
		launchPath:    "/launch/" + strconv.Itoa(listener.Addr().(*net.TCPAddr).Port) + "/" + capability,
		bootstrapHash: sha256.Sum256([]byte(capability)),
		done:          make(chan struct{}),
		sessions:      make(map[[32]byte]time.Time),
		now:           time.Now,
	}
	local.origin = "http://" + local.address
	local.http = &http.Server{
		Handler:           local,
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       10 * time.Second,
		IdleTimeout:       30 * time.Second,
		MaxHeaderBytes:    8192,
	}
	s.local = local
	go func() {
		defer close(local.done)
		_ = local.http.Serve(listener)
	}()
	return nil
}

// Close waits for local requests to finish before the native bridge is released.
func (s *Server) Close() {
	s.closeOnce.Do(func() {
		s.mu.Lock()
		s.closed = true
		local := s.local
		s.mu.Unlock()
		s.history.close()
		if local == nil {
			return
		}
		local.mu.Lock()
		local.closed = true
		local.bootstrapHash = [32]byte{}
		clear(local.sessions)
		local.mu.Unlock()
		_ = local.http.Shutdown(context.Background())
		<-local.done
	})
}

func randomToken() (string, error) {
	var token [32]byte
	if _, err := rand.Read(token[:]); err != nil {
		return "", err
	}
	return hex.EncodeToString(token[:]), nil
}

// Session creation belongs to the loopback API. The core's resource routes
// serve embedded files only and do not create or refresh sessions.
func (l *localServer) createSession(capability string) managementResponse {
	l.mu.Lock()
	defer l.mu.Unlock()
	hash := sha256.Sum256([]byte(capability))
	if l.closed || len(capability) != 64 || subtle.ConstantTimeCompare(hash[:], l.bootstrapHash[:]) != 1 {
		return jsonResponse(401, map[string]string{"error": "Reopen Account Warmup from the Plugins menu."})
	}
	now := l.now()
	for token, expires := range l.sessions {
		if !expires.After(now) {
			delete(l.sessions, token)
		}
	}
	if len(l.sessions) >= maxSessions {
		var oldest [32]byte
		var firstExpiry time.Time
		for token, expires := range l.sessions {
			if firstExpiry.IsZero() || expires.Before(firstExpiry) {
				oldest, firstExpiry = token, expires
			}
		}
		delete(l.sessions, oldest)
	}
	token, err := randomToken()
	if err != nil {
		return jsonResponse(503, map[string]string{"error": "Could not open Account Warmup."})
	}
	l.sessions[sha256.Sum256([]byte(token))] = now.Add(sessionLifetime)
	return jsonResponse(http.StatusOK, map[string]string{"session_token": token})
}

func (l *localServer) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	writeHTTPResponse(w, l.handle(r))
}

func (l *localServer) handle(r *http.Request) managementResponse {
	if r.Host != l.address {
		return jsonResponse(403, map[string]string{"error": "Invalid host."})
	}
	if origin := r.Header.Get("Origin"); origin != "" && origin != l.origin {
		return jsonResponse(403, map[string]string{"error": "Cross-origin requests are not accepted."})
	}
	if r.Method == http.MethodGet {
		switch r.URL.Path {
		case "/":
			return assetResponse("index.html", "text/html; charset=utf-8")
		case "/app.js":
			return assetResponse("app.js", "text/javascript; charset=utf-8")
		case "/styles.css":
			return assetResponse("styles.css", "text/css; charset=utf-8")
		case "/claude.svg", "/codex.svg":
			return assetResponse(strings.TrimPrefix(r.URL.Path, "/"), "image/svg+xml")
		}
	}
	if r.URL.Path == "/api/session" {
		if r.Method != http.MethodPost {
			return jsonResponse(405, map[string]string{"error": "Method not allowed."})
		}
		if origin, ok := oneHeader(r.Header, "Origin"); !ok || origin != l.origin {
			return jsonResponse(403, map[string]string{"error": "A same-origin session request is required."})
		}
		if site, ok := oneHeader(r.Header, "Sec-Fetch-Site"); !ok || site != "same-origin" {
			return jsonResponse(403, map[string]string{"error": "A same-origin session request is required."})
		}
		if r.URL.RawQuery != "" || r.URL.ForceQuery {
			return jsonResponse(400, map[string]string{"error": "Session requests must not contain a query."})
		}
		if r.Body != nil {
			body, err := io.ReadAll(io.LimitReader(r.Body, 1))
			if err != nil || len(body) != 0 {
				return jsonResponse(400, map[string]string{"error": "Session requests must have an empty body."})
			}
		}
		authorization, single := oneHeader(r.Header, "Authorization")
		scheme, capability, ok := strings.Cut(authorization, " ")
		if !single || !ok || !strings.EqualFold(scheme, "Bearer") {
			return jsonResponse(401, map[string]string{"error": "Reopen Account Warmup from the Plugins menu."})
		}
		return l.createSession(capability)
	}
	if r.URL.Path != "/api/state" && r.URL.Path != "/api/warmup" {
		return jsonResponse(404, map[string]string{"error": "Route not found."})
	}
	if !l.authorized(r.Header) {
		return jsonResponse(401, map[string]string{"error": "Reopen Account Warmup from the Plugins menu."})
	}
	if r.Header.Get("Sec-Fetch-Site") == "cross-site" {
		return jsonResponse(403, map[string]string{"error": "Cross-site requests are not accepted."})
	}
	if (r.URL.Path == "/api/state" && r.Method != http.MethodGet) ||
		(r.URL.Path == "/api/warmup" && r.Method != http.MethodPost) {
		return jsonResponse(405, map[string]string{"error": "Method not allowed."})
	}
	var body []byte
	if r.Body != nil {
		var err error
		body, err = io.ReadAll(io.LimitReader(r.Body, maxRequestBody+1))
		if err != nil {
			return jsonResponse(400, map[string]string{"error": "Could not read request."})
		}
		if len(body) > maxRequestBody {
			return jsonResponse(413, map[string]string{"error": "Request body is too large."})
		}
	}
	return l.plugin.action(managementRequest{
		Method: r.Method, Path: APIBase + strings.TrimPrefix(r.URL.Path, "/api"),
		Headers: r.Header, Body: body,
	})
}

func (l *localServer) authorized(headers http.Header) bool {
	values := headers.Values("Authorization")
	if len(values) != 1 {
		return false
	}
	scheme, token, ok := strings.Cut(values[0], " ")
	if !ok || !strings.EqualFold(scheme, "Bearer") || len(token) != 64 {
		return false
	}
	hash := sha256.Sum256([]byte(token))
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.closed {
		return false
	}
	expires, ok := l.sessions[hash]
	if !ok || !expires.After(l.now()) {
		delete(l.sessions, hash)
		return false
	}
	return true
}

func assetResponse(name, contentType string) managementResponse {
	body, err := assets.ReadFile("web/" + name)
	if err != nil {
		return jsonResponse(500, map[string]string{"error": "Asset unavailable."})
	}
	return managementResponse{StatusCode: 200, Headers: responseHeaders(contentType), Body: body}
}

func writeHTTPResponse(w http.ResponseWriter, response managementResponse) {
	for name, values := range response.Headers {
		for _, value := range values {
			w.Header().Add(name, value)
		}
	}
	w.WriteHeader(response.StatusCode)
	_, _ = w.Write(response.Body)
}
