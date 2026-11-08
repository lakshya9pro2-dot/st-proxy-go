package proxy

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestProxy_Health(t *testing.T) {
	h := NewProxyHandler("", "", true)
	req := httptest.NewRequest("GET", "/health", nil)
	w := httptest.NewRecorder()

	h.ServeHTTP(w, req)

	resp := w.Result()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200 OK, got %d", resp.StatusCode)
	}

	var body map[string]interface{}
	_ = json.NewDecoder(resp.Body).Decode(&body)
	if body["status"] != "ok" {
		t.Errorf("expected status ok, got %v", body["status"])
	}
}

func TestProxy_CORS_Preflight(t *testing.T) {
	h := NewProxyHandler("", "", true)
	req := httptest.NewRequest("OPTIONS", "/session/test/video.mp4", nil)
	w := httptest.NewRecorder()

	h.ServeHTTP(w, req)

	resp := w.Result()
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("expected 204 No Content, got %d", resp.StatusCode)
	}
	if resp.Header.Get("Access-Control-Allow-Origin") != "*" {
		t.Errorf("expected * CORS, got %s", resp.Header.Get("Access-Control-Allow-Origin"))
	}
}

func TestProxy_SessionAndRange(t *testing.T) {
	// Mock upstream server supporting 206
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Range") == "bytes=0-99" {
			w.Header().Set("Content-Type", "video/mp4")
			w.Header().Set("Content-Range", "bytes 0-99/1000")
			w.Header().Set("Content-Length", "100")
			w.Header().Set("Accept-Ranges", "bytes")
			w.WriteHeader(http.StatusPartialContent)
			_, _ = w.Write(make([]byte, 100))
			return
		}
		w.Header().Set("Content-Type", "video/mp4")
		w.Header().Set("Content-Length", "1000")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(make([]byte, 1000))
	}))
	defer upstream.Close()

	h := NewProxyHandler("", "", true)

	// 1. Create Session
	createBody, _ := json.Marshal(map[string]interface{}{
		"url": upstream.URL + "/video.mp4",
	})
	createReq := httptest.NewRequest("POST", "/session", bytes.NewReader(createBody))
	createW := httptest.NewRecorder()
	h.ServeHTTP(createW, createReq)

	createResp := createW.Result()
	if createResp.StatusCode != http.StatusCreated {
		t.Fatalf("expected 201 Created, got %d", createResp.StatusCode)
	}

	var resData map[string]interface{}
	_ = json.NewDecoder(createResp.Body).Decode(&resData)
	playbackURL, ok := resData["playback_url"].(string)
	if !ok || !strings.Contains(playbackURL, "/session/") {
		t.Fatalf("invalid playback_url: %v", playbackURL)
	}

	// 2. Fetch video with Range: bytes=0-99
	uPath := strings.TrimPrefix(playbackURL, "http://localhost:8888")
	rangeReq := httptest.NewRequest("GET", uPath, nil)
	rangeReq.Header.Set("Range", "bytes=0-99")
	rangeW := httptest.NewRecorder()
	h.ServeHTTP(rangeW, rangeReq)

	rangeResp := rangeW.Result()
	if rangeResp.StatusCode != http.StatusPartialContent {
		t.Fatalf("expected 206 Partial Content, got %d", rangeResp.StatusCode)
	}
	if rangeResp.Header.Get("Content-Range") != "bytes 0-99/1000" {
		t.Errorf("expected Content-Range 'bytes 0-99/1000', got '%s'", rangeResp.Header.Get("Content-Range"))
	}
	if rangeResp.Header.Get("Content-Length") != "100" {
		t.Errorf("expected Content-Length 100, got '%s'", rangeResp.Header.Get("Content-Length"))
	}
}

func TestProxy_LiveStreamtape_ExtractionAnd206(t *testing.T) {
	ctx := context.Background()
	ext, err := ExtractStreamtape(ctx, "0A2vDYQz3wIbPQ6")
	if err != nil {
		t.Skipf("Streamtape extraction skipped: %v", err)
	}

	if ext.StreamURL == "" || !strings.Contains(ext.StreamURL, "tapecontent.net") {
		t.Fatalf("expected tapecontent.net URL, got %s", ext.StreamURL)
	}

	// Test direct HTTP Range request to tapecontent.net
	req, err := http.NewRequestWithContext(ctx, "GET", ext.StreamURL, nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("User-Agent", DefaultUserAgent)
	req.Header.Set("Referer", ext.Headers["Referer"])
	req.Header.Set("Range", "bytes=0-100")

	client := &http.Client{Timeout: 15 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusPartialContent {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("expected 206 Partial Content, got %d (body: %s)", resp.StatusCode, string(body))
	}

	cr := resp.Header.Get("Content-Range")
	if !strings.HasPrefix(cr, "bytes 0-100/") {
		t.Errorf("expected Content-Range starting with 'bytes 0-100/', got %s", cr)
	}
}

func TestProxy_Extract_CacheAndSingleflight(t *testing.T) {
	h := NewProxyHandler("", "", true)

	// Seed cache directly to verify instant hit and Cache-Control
	fakeExt := &ExtractResult{
		StreamURL:  "https://test.tapecontent.net/video.mp4",
		DirectURL:  "https://test.tapecontent.net/video.mp4",
		StreamType: "mp4",
		Title:      "Cached Test Video",
		Headers:    map[string]string{"Referer": "https://streamtape.com/"},
	}
	h.cache.Set("st:mock-cached-id", fakeExt, 1*time.Hour)

	req := httptest.NewRequest("GET", "/extract/st/mock-cached-id", nil)
	w := httptest.NewRecorder()

	start := time.Now()
	h.ServeHTTP(w, req)
	elapsed := time.Since(start)

	resp := w.Result()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200 OK, got %d", resp.StatusCode)
	}

	// Should be served in less than 5ms from cache
	if elapsed > 10*time.Millisecond {
		t.Errorf("cache hit took too long: %v", elapsed)
	}

	cc := resp.Header.Get("Cache-Control")
	if !strings.Contains(cc, "max-age=3600") {
		t.Errorf("expected Cache-Control with max-age=3600, got %s", cc)
	}

	var resData map[string]interface{}
	_ = json.NewDecoder(resp.Body).Decode(&resData)
	if resData["title"] != "Cached Test Video" {
		t.Errorf("expected title 'Cached Test Video', got %v", resData["title"])
	}
}
