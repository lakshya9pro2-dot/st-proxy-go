package proxy

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"
)

// ProxyHandler is the HTTP handler for the Netlify Go proxy.
type ProxyHandler struct {
	sessions          *SessionManager
	client            *http.Client
	secret            string
	allowedDomains    string
	allowInsecureTest bool
}

// NewProxyHandler creates a new ProxyHandler instance.
func NewProxyHandler(secret, allowedDomains string, allowInsecureTest bool) *ProxyHandler {
	return &ProxyHandler{
		sessions: NewSessionManager(secret, DefaultSessionTTL),
		client: &http.Client{
			Timeout: 30 * time.Second,
		},
		secret:            secret,
		allowedDomains:    allowedDomains,
		allowInsecureTest: allowInsecureTest,
	}
}

// ServeHTTP implements http.Handler.
func (h *ProxyHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	AddCORSHeaders(w)

	// 1. CORS Preflight
	if r.Method == http.MethodOptions {
		w.WriteHeader(http.StatusNoContent)
		return
	}

	path := r.URL.Path
	// Strip Netlify function prefix if forwarded
	prefix := "/.netlify/functions/proxy"
	if strings.HasPrefix(path, prefix) {
		path = strings.TrimPrefix(path, prefix)
		if path == "" {
			path = "/"
		}
	}

	proxyBaseURL := h.deriveBaseURL(r)

	// 2. Health check
	if path == "/health" || path == "/api/health" {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]interface{}{
			"status":    "ok",
			"service":   "netlify-go-playback-proxy",
			"timestamp": time.Now().UnixMilli(),
		})
		return
	}

	// 3. Create Session: POST /session or POST /api/session
	if r.Method == http.MethodPost && (path == "/session" || path == "/api/session" || path == "/") {
		h.handleCreateSession(w, r, proxyBaseURL)
		return
	}

	// 3A. Extract POST: POST /extract, POST /api/extract
	if r.Method == http.MethodPost && (path == "/extract" || path == "/api/extract" || path == "/session/extract") {
		h.handleExtractPost(w, r, proxyBaseURL)
		return
	}

	// 3B. Extract GET: GET /extract/:provider/:id
	if r.Method == http.MethodGet && strings.HasPrefix(path, "/extract/") {
		parts := strings.Split(strings.TrimPrefix(path, "/extract/"), "/")
		if len(parts) >= 2 && parts[0] != "" && parts[1] != "" {
			h.handleExtractGet(w, r, parts[0], parts[1], proxyBaseURL)
			return
		}
	}

	// 4. Session Routes: /session/:sessionId/...
	if strings.HasPrefix(path, "/session/") {
		rest := strings.TrimPrefix(path, "/session/")
		slashIdx := strings.Index(rest, "/")
		if slashIdx == -1 {
			http.Error(w, `{"error":"invalid_session_path"}`, http.StatusBadRequest)
			return
		}

		sessionID := rest[:slashIdx]
		subPath := rest[slashIdx:]

		session, err := h.sessions.GetSession(sessionID)
		if err != nil {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusGone)
			_ = json.NewEncoder(w).Encode(map[string]string{
				"error":   "session_expired_or_not_found",
				"message": err.Error(),
			})
			return
		}

		// 4A. Master Playlist
		if subPath == "/master.m3u8" || subPath == "/index.m3u8" || subPath == "/playlist.m3u8" {
			h.handleFetchAndRewritePlaylist(w, r, session.URL, sessionID, session, proxyBaseURL)
			return
		}

		// 4B. Direct MP4 / Video stream
		if subPath == "/video.mp4" || subPath == "/stream.mp4" || subPath == "/video" || subPath == "/stream" || subPath == "/play" || strings.HasSuffix(subPath, ".mp4") || strings.HasSuffix(subPath, ".mkv") {
			h.handleProxyResource(w, r, session.URL, session)
			return
		}

		// 4C. Proxied Resource: /session/:id/res/:token/:filename
		if strings.HasPrefix(subPath, "/res/") {
			resParts := strings.Split(strings.TrimPrefix(subPath, "/res/"), "/")
			if len(resParts) > 0 && resParts[0] != "" {
				targetURL, decErr := DecodeResourceToken(resParts[0])
				if decErr != nil {
					http.Error(w, `{"error":"invalid_resource_token"}`, http.StatusBadRequest)
					return
				}
				h.handleProxyResource(w, r, targetURL, session)
				return
			}
		}

		// 4D. Relative fallback
		target := strings.TrimPrefix(subPath, "/")
		if target != "" && !strings.Contains(target, "/") {
			resolved := ResolveURI(target, session.URL)
			h.handleProxyResource(w, r, resolved, session)
			return
		}
	}

	http.NotFound(w, r)
}

func (h *ProxyHandler) deriveBaseURL(r *http.Request) string {
	scheme := "https"
	if r.TLS == nil && !strings.EqualFold(r.Header.Get("X-Forwarded-Proto"), "https") {
		scheme = "http"
	}
	host := r.Host
	if host == "" {
		host = "localhost:8888"
	}
	return fmt.Sprintf("%s://%s", scheme, host)
}

func (h *ProxyHandler) handleCreateSession(w http.ResponseWriter, r *http.Request, proxyBaseURL string) {
	if !VerifyWorkerAuth(r, h.secret) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusUnauthorized)
		_ = json.NewEncoder(w).Encode(map[string]string{"error": "unauthorized"})
		return
	}

	var reqBody struct {
		URL     string            `json:"url"`
		Headers map[string]string `json:"headers"`
		VideoID string            `json:"videoId"`
		Type    string            `json:"type"`
	}

	if err := json.NewDecoder(r.Body).Decode(&reqBody); err != nil || reqBody.URL == "" {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		_ = json.NewEncoder(w).Encode(map[string]string{"error": "missing_upstream_url"})
		return
	}

	if _, err := ValidateUpstreamURL(reqBody.URL, h.allowedDomains, h.allowInsecureTest); err != nil {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusForbidden)
		_ = json.NewEncoder(w).Encode(map[string]string{"error": "disallowed_upstream_url", "message": err.Error()})
		return
	}

	sessionID, expiresAt, err := h.sessions.CreateSession(reqBody.URL, SanitizeUpstreamHeaders(reqBody.Headers), reqBody.VideoID, reqBody.Type, "")
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	isDirect := strings.Contains(reqBody.URL, ".mp4") || strings.Contains(reqBody.URL, ".mkv") || strings.Contains(reqBody.URL, "tapecontent.net")
	suffix := "master.m3u8"
	if isDirect {
		suffix = "video.mp4"
	}

	playbackURL := fmt.Sprintf("%s/session/%s/%s", strings.TrimRight(proxyBaseURL, "/"), sessionID, suffix)

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusCreated)
	_ = json.NewEncoder(w).Encode(map[string]interface{}{
		"success":      true,
		"session_id":   sessionID,
		"playback_url": playbackURL,
		"expires_at":   expiresAt,
	})
}

func (h *ProxyHandler) handleExtractPost(w http.ResponseWriter, r *http.Request, proxyBaseURL string) {
	if !VerifyWorkerAuth(r, h.secret) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusUnauthorized)
		_ = json.NewEncoder(w).Encode(map[string]string{"error": "unauthorized"})
		return
	}

	var reqBody struct {
		Provider string `json:"provider"`
		ID       string `json:"id"`
		VideoID  string `json:"videoId"`
		Type     string `json:"type"`
	}

	if err := json.NewDecoder(r.Body).Decode(&reqBody); err != nil || reqBody.Provider == "" || reqBody.ID == "" {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		_ = json.NewEncoder(w).Encode(map[string]string{"error": "missing_parameters"})
		return
	}

	h.executeExtraction(w, r.Context(), reqBody.Provider, reqBody.ID, reqBody.VideoID, reqBody.Type, proxyBaseURL)
}

func (h *ProxyHandler) handleExtractGet(w http.ResponseWriter, r *http.Request, provider, id, proxyBaseURL string) {
	if !VerifyWorkerAuth(r, h.secret) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusUnauthorized)
		_ = json.NewEncoder(w).Encode(map[string]string{"error": "unauthorized"})
		return
	}

	q := r.URL.Query()
	videoID := q.Get("videoId")
	if videoID == "" {
		videoID = id
	}
	mediaType := q.Get("type")
	if mediaType == "" {
		mediaType = "movie"
	}

	h.executeExtraction(w, r.Context(), provider, id, videoID, mediaType, proxyBaseURL)
}

func (h *ProxyHandler) executeExtraction(w http.ResponseWriter, ctx context.Context, provider, id, videoID, mediaType, proxyBaseURL string) {
	var ext *ExtractResult
	var err error

	prov := strings.ToLower(strings.TrimSpace(provider))
	if prov == "streamhg" || prov == "hanerix" || prov == "sh" {
		ext, err = ExtractStreamHG(ctx, id)
	} else if prov == "streamtape" || prov == "st" {
		ext, err = ExtractStreamtape(ctx, id)
	} else {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		_ = json.NewEncoder(w).Encode(map[string]string{
			"error":   "unsupported_provider",
			"message": fmt.Sprintf("Provider '%s' not supported", provider),
		})
		return
	}

	if err != nil {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadGateway)
		_ = json.NewEncoder(w).Encode(map[string]string{
			"error":   "extraction_failed",
			"message": err.Error(),
		})
		return
	}

	sessionID, expiresAt, err := h.sessions.CreateSession(ext.StreamURL, SanitizeUpstreamHeaders(ext.Headers), videoID, mediaType, prov)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	suffix := "master.m3u8"
	if ext.StreamType == "mp4" {
		suffix = "video.mp4"
	}

	playbackURL := fmt.Sprintf("%s/session/%s/%s", strings.TrimRight(proxyBaseURL, "/"), sessionID, suffix)

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]interface{}{
		"success":         true,
		"provider":        prov,
		"session_id":      sessionID,
		"playback_url":    playbackURL,
		"url":             playbackURL,
		"direct_url":      ext.DirectURL,
		"tapecontent_url": ext.TapecontentURL,
		"get_video_url":   ext.GetVideoURL,
		"stream_type":     ext.StreamType,
		"title":           ext.Title,
		"video_id":        id,
		"type":            mediaType,
		"expires_at":      expiresAt,
	})
}

func (h *ProxyHandler) handleFetchAndRewritePlaylist(w http.ResponseWriter, r *http.Request, targetURL, sessionID string, session *SessionData, proxyBaseURL string) {
	req, err := http.NewRequestWithContext(r.Context(), "GET", targetURL, nil)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadGateway)
		return
	}

	for k, v := range session.Headers {
		req.Header.Set(k, v)
	}
	if req.Header.Get("User-Agent") == "" {
		req.Header.Set("User-Agent", DefaultUserAgent)
	}

	resp, err := h.client.Do(req)
	if err != nil {
		http.Error(w, fmt.Sprintf("Failed to fetch upstream playlist: %v", err), http.StatusBadGateway)
		return
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		w.WriteHeader(resp.StatusCode)
		return
	}

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadGateway)
		return
	}

	rewritten := RewritePlaylist(string(body), targetURL, sessionID, proxyBaseURL)

	w.Header().Set("Content-Type", "application/vnd.apple.mpegurl; charset=utf-8")
	w.Header().Set("Cache-Control", "no-cache, no-store, must-revalidate")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte(rewritten))
}

func (h *ProxyHandler) handleProxyResource(w http.ResponseWriter, r *http.Request, targetURL string, session *SessionData) {
	req, err := http.NewRequestWithContext(r.Context(), r.Method, targetURL, nil)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadGateway)
		return
	}

	for k, v := range session.Headers {
		req.Header.Set(k, v)
	}
	if req.Header.Get("User-Agent") == "" {
		req.Header.Set("User-Agent", DefaultUserAgent)
	}

	// Forward client Range header
	if rg := r.Header.Get("Range"); rg != "" {
		req.Header.Set("Range", rg)
	}

	resp, err := h.client.Do(req)
	if err != nil {
		http.Error(w, fmt.Sprintf("Failed to fetch upstream resource: %v", err), http.StatusBadGateway)
		return
	}
	defer resp.Body.Close()

	for _, k := range []string{"Content-Range", "Content-Length", "Content-Type", "Accept-Ranges", "Last-Modified", "ETag"} {
		if v := resp.Header.Get(k); v != "" {
			w.Header().Set(k, v)
		}
	}
	w.Header().Set("Accept-Ranges", "bytes")

	w.WriteHeader(resp.StatusCode)
	if r.Method == http.MethodHead {
		return
	}

	buf := make([]byte, 128*1024)
	_, _ = io.CopyBuffer(w, resp.Body, buf)
}

// EnvOrDefault gets environment variable or returns fallback.
func EnvOrDefault(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}
