package proxy

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"
)

// MaxServerlessChunkSize is the maximum video chunk size (2MB) proxied per request.
// Prevents AWS Lambda / Netlify 30-second timeouts and 6MB payload limits on multi-gigabyte video files.
const MaxServerlessChunkSize int64 = 2 * 1024 * 1024

// ProxyHandler is the HTTP handler for the Netlify Go proxy.
type ProxyHandler struct {
	sessions          *SessionManager
	client            *http.Client
	secret            string
	allowedDomains    string
	allowInsecureTest bool
	cache             *MemoryCache
	sf                *SingleflightGroup
	bufferPool        sync.Pool
}

// NewProxyHandler creates a new ProxyHandler instance.
func NewProxyHandler(secret, allowedDomains string, allowInsecureTest bool) *ProxyHandler {
	transport := &http.Transport{
		MaxIdleConns:          500,
		MaxIdleConnsPerHost:   100,
		IdleConnTimeout:       90 * time.Second,
		TLSHandshakeTimeout:   5 * time.Second,
		ResponseHeaderTimeout: 15 * time.Second,
		ExpectContinueTimeout: 1 * time.Second,
		ForceAttemptHTTP2:     true,
		DisableCompression:    true, // Do not compress video streams - wastes CPU!
	}

	return &ProxyHandler{
		sessions: NewSessionManager(secret, DefaultSessionTTL),
		client: &http.Client{
			Transport: transport,
			Timeout:   30 * time.Second,
		},
		secret:            secret,
		allowedDomains:    allowedDomains,
		allowInsecureTest: allowInsecureTest,
		cache:             NewMemoryCache(),
		sf:                NewSingleflightGroup(),
		bufferPool: sync.Pool{
			New: func() interface{} {
				b := make([]byte, 128*1024)
				return &b
			},
		},
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

	clientIP := r.Header.Get("X-Forwarded-For")
	if clientIP == "" {
		clientIP = r.RemoteAddr
	}
	log.Printf("[req] %s %s client_ip=%s range=%s", r.Method, path, clientIP, r.Header.Get("Range"))

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
		host = r.Header.Get("X-Forwarded-Host")
	}
	if host == "" {
		host = r.Header.Get("Host")
	}
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
	prov := strings.ToLower(strings.TrimSpace(provider))
	if prov != "streamhg" && prov != "hanerix" && prov != "sh" && prov != "streamtape" && prov != "st" {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		_ = json.NewEncoder(w).Encode(map[string]string{
			"error":   "unsupported_provider",
			"message": fmt.Sprintf("Provider '%s' not supported", provider),
		})
		return
	}

	cleanID := strings.TrimSpace(id)
	cacheKey := fmt.Sprintf("%s:%s", prov, cleanID)

	var ext *ExtractResult
	if cachedVal, ok := h.cache.Get(cacheKey); ok {
		if res, castOk := cachedVal.(*ExtractResult); castOk && res != nil {
			ext = res
			log.Printf("[extract] in-memory cache hit for key=%s", cacheKey)
		}
	}

	if ext == nil {
		log.Printf("[extract] cache miss for key=%s, initiating extraction with singleflight", cacheKey)
		resVal, err := h.sf.Do(cacheKey, func() (interface{}, error) {
			if prov == "streamhg" || prov == "hanerix" || prov == "sh" {
				return ExtractStreamHG(ctx, cleanID)
			}
			return ExtractStreamtape(ctx, cleanID)
		})

		if err != nil {
			log.Printf("[extract] extraction failed for %s (%s): %v", prov, cleanID, err)
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusBadGateway)
			_ = json.NewEncoder(w).Encode(map[string]string{
				"error":   "extraction_failed",
				"message": err.Error(),
			})
			return
		}

		ext = resVal.(*ExtractResult)
		// Cache for 2 hours (upstream stream tokens valid for 4-12 hours)
		h.cache.Set(cacheKey, ext, 2*time.Hour)
		log.Printf("[extract] cached extraction for key=%s (ttl=2h)", cacheKey)
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
	// Cache-Control: tell Netlify Edge CDN and browsers to cache extract response
	w.Header().Set("Cache-Control", "public, max-age=3600, s-maxage=7200, stale-while-revalidate=600")
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
	w.Header().Set("Cache-Control", "public, max-age=10, must-revalidate")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte(rewritten))
}

func (h *ProxyHandler) handleProxyResource(w http.ResponseWriter, r *http.Request, targetURL string, session *SessionData) {
	start := time.Now()
	clientRange := r.Header.Get("Range")

	req, err := http.NewRequestWithContext(r.Context(), r.Method, targetURL, nil)
	if err != nil {
		log.Printf("[proxy] failed to create request for %s: %v", targetURL, err)
		http.Error(w, err.Error(), http.StatusBadGateway)
		return
	}

	for k, v := range session.Headers {
		req.Header.Set(k, v)
	}
	if req.Header.Get("User-Agent") == "" {
		req.Header.Set("User-Agent", DefaultUserAgent)
	}

	// For direct MP4/video streaming, apply safe chunk clamping to prevent Lambda 30s timeouts
	isDirectVideo := strings.Contains(targetURL, ".mp4") || strings.Contains(targetURL, ".mkv") || strings.Contains(targetURL, "tapecontent.net")
	appliedRange := clientRange
	if isDirectVideo && r.Method != http.MethodHead {
		clamped, modified := ClampRange(clientRange, MaxServerlessChunkSize)
		appliedRange = clamped
		if modified {
			log.Printf("[proxy] clamped range for serverless payload: client=%q -> upstream=%q", clientRange, clamped)
		}
	}

	if appliedRange != "" {
		req.Header.Set("Range", appliedRange)
	}

	log.Printf("[proxy] fetching upstream target=%s method=%s range=%s", targetURL, r.Method, appliedRange)

	resp, err := h.client.Do(req)
	if err != nil {
		log.Printf("[proxy] upstream request failed after %v: %v", time.Since(start), err)
		http.Error(w, fmt.Sprintf("Failed to fetch upstream resource: %v", err), http.StatusBadGateway)
		return
	}
	defer resp.Body.Close()

	log.Printf("[proxy] upstream responded in %v: status=%d content_length=%s content_range=%s content_type=%s",
		time.Since(start), resp.StatusCode, resp.Header.Get("Content-Length"), resp.Header.Get("Content-Range"), resp.Header.Get("Content-Type"))

	for _, k := range []string{"Content-Range", "Content-Length", "Content-Type", "Accept-Ranges", "Last-Modified", "ETag"} {
		if v := resp.Header.Get(k); v != "" {
			w.Header().Set(k, v)
		}
	}
	w.Header().Set("Accept-Ranges", "bytes")
	if isDirectVideo || strings.HasSuffix(r.URL.Path, ".ts") || strings.HasSuffix(r.URL.Path, ".m4s") {
		w.Header().Set("Cache-Control", "public, max-age=86400, immutable")
	}

	w.WriteHeader(resp.StatusCode)
	if r.Method == http.MethodHead {
		return
	}

	// Limit reader to at most MaxServerlessChunkSize + 128KB to prevent memory exhaustion or hangs
	var src io.Reader = resp.Body
	if isDirectVideo {
		src = io.LimitReader(resp.Body, MaxServerlessChunkSize+128*1024)
	}

	bufPtr := h.bufferPool.Get().(*[]byte)
	defer h.bufferPool.Put(bufPtr)
	buf := *bufPtr

	n, copyErr := io.CopyBuffer(w, src, buf)
	if copyErr != nil {
		log.Printf("[proxy] copy buffer error after %d bytes: %v", n, copyErr)
	} else {
		log.Printf("[proxy] completed stream transfer: copied=%d bytes total_time=%v", n, time.Since(start))
	}
}

// ClampRange calculates safe serverless range header to prevent 30-second timeouts on multi-gigabyte video files.
func ClampRange(rawRange string, maxChunk int64) (string, bool) {
	rawRange = strings.TrimSpace(rawRange)
	if rawRange == "" {
		// No range provided: request initial chunk 0-(maxChunk-1)
		return fmt.Sprintf("bytes=0-%d", maxChunk-1), true
	}

	if !strings.HasPrefix(strings.ToLower(rawRange), "bytes=") {
		return rawRange, false
	}

	spec := strings.TrimPrefix(rawRange, "bytes=")
	parts := strings.Split(spec, "-")
	if len(parts) != 2 {
		return rawRange, false
	}

	startStr := strings.TrimSpace(parts[0])
	endStr := strings.TrimSpace(parts[1])

	if startStr == "" {
		// Suffix range: bytes=-500 (last 500 bytes)
		return rawRange, false
	}

	start, err := strconv.ParseInt(startStr, 10, 64)
	if err != nil || start < 0 {
		return rawRange, false
	}

	if endStr == "" {
		// Open range: bytes=start- -> clamp to start + maxChunk - 1
		end := start + maxChunk - 1
		return fmt.Sprintf("bytes=%d-%d", start, end), true
	}

	end, err := strconv.ParseInt(endStr, 10, 64)
	if err != nil || end < start {
		return rawRange, false
	}

	if end-start+1 > maxChunk {
		end = start + maxChunk - 1
		return fmt.Sprintf("bytes=%d-%d", start, end), true
	}

	return rawRange, false
}

// EnvOrDefault gets environment variable or returns fallback.
func EnvOrDefault(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}
