package proxy

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"
)

const (
	DefaultStreamHGBase   = "https://hanerix.com"
	DefaultStreamtapeBase = "https://streamtape.com"
	DefaultUserAgent      = "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/124.0.0.0 Safari/537.36"
)

var (
	reStreamtapeCode = regexp.MustCompile(`[^a-zA-Z0-9_\-]`)
	reBotlink        = regexp.MustCompile(`(?i)botlink['"]\)\.innerHTML\s*=\s*([^;]+);`)
	reRobotlink      = regexp.MustCompile(`(?i)getElementById\(['"](?:no)?robotlink['"]\)\.innerHTML\s*=\s*([^;]+);`)
	reTitle          = regexp.MustCompile(`(?i)<title>(.*?)</title>`)
	reVarSrc         = regexp.MustCompile(`(?i)var\s+src\s*=\s*"([^"]+)"`)
	reSourcesFile    = regexp.MustCompile(`(?i)file\s*:\s*["']([^"']+)["']`)
	reM3U8Any        = regexp.MustCompile(`(?i)["'](https?://[^"']+\.m3u8[^"']*)["']`)
	reChainMethod    = regexp.MustCompile(`\.(substring|slice)\(\s*(\d+)(?:\s*,\s*(\d+))?\s*\)`)
	rePartString     = regexp.MustCompile(`^\(?\s*['"]([\s\S]*?)['"]\s*\)?(.*)$`)
)

// ExtractResult holds stream extraction details.
type ExtractResult struct {
	StreamURL      string            `json:"stream_url"`
	DirectURL      string            `json:"direct_url"`
	TapecontentURL string            `json:"tapecontent_url,omitempty"`
	GetVideoURL    string            `json:"get_video_url,omitempty"`
	StreamType     string            `json:"stream_type"`
	Title          string            `json:"title,omitempty"`
	Headers        map[string]string `json:"headers"`
}

// ExtractStreamHG extracts HLS stream from StreamHG / Hanerix.
func ExtractStreamHG(ctx context.Context, fileCodeOrURL string) (*ExtractResult, error) {
	fileCode := strings.TrimSpace(fileCodeOrURL)
	embedURL := ""

	if strings.HasPrefix(fileCode, "http://") || strings.HasPrefix(fileCode, "https://") {
		embedURL = fileCode
		m := regexp.MustCompile(`/(?:e|v|embed)/([a-zA-Z0-9_\-]+)`).FindStringSubmatch(fileCode)
		if len(m) > 1 {
			fileCode = m[1]
		}
	} else {
		fileCode = reStreamtapeCode.ReplaceAllString(fileCode, "")
		if fileCode == "" {
			return nil, fmt.Errorf("invalid or empty StreamHG ID")
		}
		embedURL = fmt.Sprintf("%s/e/%s", DefaultStreamHGBase, fileCode)
	}

	u, _ := url.Parse(embedURL)
	embedOrigin := fmt.Sprintf("%s://%s", u.Scheme, u.Host)

	req, err := http.NewRequestWithContext(ctx, "GET", embedURL, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", DefaultUserAgent)
	req.Header.Set("Referer", embedOrigin+"/")
	req.Header.Set("Accept", "text/html,application/xhtml+xml,application/xml;q=0.9,*/*;q=0.8")

	client := &http.Client{Timeout: 15 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("streamhg request failed: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("streamhg returned HTTP %d", resp.StatusCode)
	}

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}
	html := string(body)

	// 1. Direct var src = "..." check
	if m := reVarSrc.FindStringSubmatch(html); len(m) > 1 && m[1] != "" {
		sURL := m[1]
		if strings.HasPrefix(sURL, "//") {
			sURL = "https:" + sURL
		}
		st := "hls"
		if strings.Contains(sURL, ".mp4") {
			st = "mp4"
		}
		return &ExtractResult{
			StreamURL:  sURL,
			DirectURL:  sURL,
			StreamType: st,
			Headers: map[string]string{
				"Referer": embedURL,
				"Origin":  embedOrigin,
			},
		}, nil
	}

	// 2. Search sources file
	if m := reSourcesFile.FindStringSubmatch(html); len(m) > 1 && m[1] != "" {
		sURL := m[1]
		if strings.HasPrefix(sURL, "//") {
			sURL = "https:" + sURL
		}
		st := "hls"
		if strings.Contains(sURL, ".mp4") {
			st = "mp4"
		}
		return &ExtractResult{
			StreamURL:  sURL,
			DirectURL:  sURL,
			StreamType: st,
			Headers: map[string]string{
				"Referer": embedURL,
				"Origin":  embedOrigin,
			},
		}, nil
	}

	// 3. Fallback search any .m3u8
	if m := reM3U8Any.FindStringSubmatch(html); len(m) > 1 && m[1] != "" {
		return &ExtractResult{
			StreamURL:  m[1],
			DirectURL:  m[1],
			StreamType: "hls",
			Headers: map[string]string{
				"Referer": embedURL,
				"Origin":  embedOrigin,
			},
		}, nil
	}

	return nil, fmt.Errorf("no playable stream found in StreamHG embed")
}

// ExtractStreamtape extracts direct MP4 video from Streamtape.
func ExtractStreamtape(ctx context.Context, fileCodeOrURL string) (*ExtractResult, error) {
	fileCode := strings.TrimSpace(fileCodeOrURL)
	pageURL := ""

	if strings.HasPrefix(fileCode, "http://") || strings.HasPrefix(fileCode, "https://") {
		pageURL = fileCode
		m := regexp.MustCompile(`/(?:v|e)/([a-zA-Z0-9_\-]+)`).FindStringSubmatch(fileCode)
		if len(m) > 1 {
			fileCode = m[1]
		}
	} else {
		fileCode = reStreamtapeCode.ReplaceAllString(fileCode, "")
		if fileCode == "" {
			return nil, fmt.Errorf("invalid or empty Streamtape ID")
		}
		pageURL = fmt.Sprintf("%s/v/%s/", DefaultStreamtapeBase, fileCode)
	}

	req, err := http.NewRequestWithContext(ctx, "GET", pageURL, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", DefaultUserAgent)
	req.Header.Set("Accept", "text/html,application/xhtml+xml,application/xml;q=0.9,*/*;q=0.8")

	client := &http.Client{Timeout: 15 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("streamtape request failed: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("streamtape returned HTTP %d", resp.StatusCode)
	}

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}
	html := string(body)

	// Streamtape token expression extraction
	matchExpr := ""
	lines := strings.Split(html, "\n")
	for _, line := range lines {
		if strings.Contains(line, "botlink').innerHTML") {
			idx := strings.Index(line, ").innerHTML")
			if idx != -1 {
				after := line[idx+len(").innerHTML"):]
				eqIdx := strings.Index(after, "=")
				if eqIdx != -1 {
					rawExpr := after[eqIdx+1:]
					if endIdx := strings.Index(rawExpr, "</script>"); endIdx != -1 {
						rawExpr = rawExpr[:endIdx]
					}
					matchExpr = strings.TrimRight(strings.TrimSpace(rawExpr), ";")
					break
				}
			}
		}
	}

	if matchExpr == "" {
		if m := reRobotlink.FindStringSubmatch(html); len(m) > 1 {
			matchExpr = strings.TrimSpace(m[1])
		} else if m := reBotlink.FindStringSubmatch(html); len(m) > 1 {
			matchExpr = strings.TrimSpace(m[1])
		}
	}

	if matchExpr == "" {
		return nil, fmt.Errorf("could not find Streamtape token expression in HTML")
	}

	evalResult := EvaluateStreamtapeExpression(matchExpr)
	if evalResult == "" {
		return nil, fmt.Errorf("failed to evaluate Streamtape token expression")
	}

	getVideoURL := evalResult
	getVideoURL = strings.ReplaceAll(getVideoURL, "/get_xcdvideo?", "/get_video?")
	getVideoURL = strings.ReplaceAll(getVideoURL, "xcdd=", "d=")
	getVideoURL = strings.ReplaceAll(getVideoURL, "/get_xcd", "/get_")

	if strings.HasPrefix(getVideoURL, "//") {
		getVideoURL = "https:" + getVideoURL
	} else if !strings.HasPrefix(getVideoURL, "http") {
		getVideoURL = "https://" + getVideoURL
	}
	if !strings.Contains(getVideoURL, "&stream=1") {
		getVideoURL += "&stream=1"
	}

	title := ""
	if m := reTitle.FindStringSubmatch(html); len(m) > 1 {
		title = strings.TrimSpace(regexp.MustCompile(`(?i)\s+at\s+Streamtape\.com.*$`).ReplaceAllString(m[1], ""))
	}

	u, _ := url.Parse(pageURL)
	streamOrigin := fmt.Sprintf("%s://%s", u.Scheme, u.Host)

	// Resolve 302 redirect from streamtape.com/get_video to the real storage URL (tapecontent.net)
	realStreamURL := getVideoURL
	noRedirectClient := &http.Client{
		Timeout: 10 * time.Second,
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
	redirReq, rErr := http.NewRequestWithContext(ctx, "GET", getVideoURL, nil)
	if rErr == nil {
		redirReq.Header.Set("User-Agent", DefaultUserAgent)
		redirReq.Header.Set("Referer", pageURL)
		redirReq.Header.Set("Range", "bytes=0-0")
		if redirResp, doErr := noRedirectClient.Do(redirReq); doErr == nil {
			defer redirResp.Body.Close()
			if redirResp.StatusCode >= 300 && redirResp.StatusCode < 400 {
				loc := redirResp.Header.Get("Location")
				if strings.HasPrefix(loc, "http://") || strings.HasPrefix(loc, "https://") {
					realStreamURL = loc
				}
			}
		}
	}

	return &ExtractResult{
		StreamURL:      realStreamURL,
		DirectURL:      realStreamURL,
		TapecontentURL: realStreamURL,
		GetVideoURL:    getVideoURL,
		StreamType:     "mp4",
		Title:          title,
		Headers: map[string]string{
			"Referer": pageURL,
			"Origin":  streamOrigin,
		},
	}, nil
}

// EvaluateStreamtapeExpression safely parses chained substrings and string concats in Go.
func EvaluateStreamtapeExpression(expr string) string {
	parts := strings.Split(expr, "+")
	var result strings.Builder

	for _, part := range parts {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}

		m := rePartString.FindStringSubmatch(part)
		if len(m) < 3 {
			continue
		}

		text := m[1]
		chain := strings.TrimSpace(m[2])

		if chain != "" {
			matches := reChainMethod.FindAllStringSubmatch(chain, -1)
			for _, match := range matches {
				method := match[1]
				start, _ := strconv.Atoi(match[2])
				hasEnd := match[3] != ""
				end := 0
				if hasEnd {
					end, _ = strconv.Atoi(match[3])
				}

				if method == "substring" {
					if start < 0 {
						start = 0
					}
					if start > len(text) {
						start = len(text)
					}
					if hasEnd {
						if end < start {
							start, end = end, start
						}
						if end > len(text) {
							end = len(text)
						}
						text = text[start:end]
					} else {
						text = text[start:]
					}
				} else if method == "slice" {
					if start < 0 {
						start = len(text) + start
						if start < 0 {
							start = 0
						}
					}
					if start > len(text) {
						start = len(text)
					}
					if hasEnd {
						if end < 0 {
							end = len(text) + end
						}
						if end < start {
							end = start
						}
						if end > len(text) {
							end = len(text)
						}
						text = text[start:end]
					} else {
						text = text[start:]
					}
				}
			}
		}
		result.WriteString(text)
	}

	return result.String()
}
