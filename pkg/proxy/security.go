package proxy

import (
	"encoding/base64"
	"fmt"
	"net/http"
	"net/url"
	"regexp"
	"strings"
)

var privateIPRegexes = []*regexp.Regexp{
	regexp.MustCompile(`^127\.\d{1,3}\.\d{1,3}\.\d{1,3}$`),
	regexp.MustCompile(`^10\.\d{1,3}\.\d{1,3}\.\d{1,3}$`),
	regexp.MustCompile(`^192\.168\.\d{1,3}\.\d{1,3}$`),
	regexp.MustCompile(`^172\.(1[6-9]|2\d|3[0-1])\.\d{1,3}\.\d{1,3}$`),
	regexp.MustCompile(`^169\.254\.\d{1,3}\.\d{1,3}$`),
	regexp.MustCompile(`^0\.0\.0\.0$`),
	regexp.MustCompile(`(?i)^localhost$`),
	regexp.MustCompile(`(?i)^\[?::1\]?$`),
	regexp.MustCompile(`(?i)^\[?fe80:`),
	regexp.MustCompile(`(?i)^\[?fc00:`),
	regexp.MustCompile(`(?i)^\[?fd00:`),
}

// ValidateUpstreamURL validates that an upstream URL is safe to fetch (strict HTTPS and SSRF protection).
func ValidateUpstreamURL(urlStr string, allowedDomainsConfig string, allowInsecureTest bool) (*url.URL, error) {
	urlStr = strings.TrimSpace(urlStr)
	if urlStr == "" {
		return nil, fmt.Errorf("missing or empty upstream URL")
	}

	parsed, err := url.Parse(urlStr)
	if err != nil {
		return nil, fmt.Errorf("malformed URL: %w", err)
	}

	hostname := strings.ToLower(parsed.Hostname())

	if allowInsecureTest && (hostname == "127.0.0.1" || hostname == "localhost") {
		return parsed, nil
	}

	if parsed.Scheme != "https" {
		return nil, fmt.Errorf("protocol must be HTTPS. Insecure HTTP is forbidden")
	}

	for _, re := range privateIPRegexes {
		if re.MatchString(hostname) {
			return nil, fmt.Errorf("access to private or local network hosts is strictly forbidden")
		}
	}

	if allowedDomainsConfig != "" {
		allowed := strings.Split(allowedDomainsConfig, ",")
		matched := false
		for _, a := range allowed {
			cleanA := strings.TrimSpace(strings.ToLower(a))
			if cleanA == "" {
				continue
			}
			if hostname == cleanA || strings.HasSuffix(hostname, "."+cleanA) {
				matched = true
				break
			}
		}
		if !matched {
			return nil, fmt.Errorf("host '%s' is not in the allowed upstream domains list", hostname)
		}
	}

	return parsed, nil
}

// VerifyWorkerAuth checks authorization against secret.
func VerifyWorkerAuth(r *http.Request, secret string) bool {
	secret = strings.TrimSpace(secret)
	if secret == "" {
		return true // open access
	}

	if k := r.Header.Get("X-Worker-Key"); k == secret {
		return true
	}
	if k := r.Header.Get("X-Proxy-Key"); k == secret {
		return true
	}

	auth := r.Header.Get("Authorization")
	if strings.HasPrefix(auth, "Bearer ") {
		token := strings.TrimSpace(strings.TrimPrefix(auth, "Bearer "))
		if token == secret {
			return true
		}
	}

	return false
}

// SanitizeUpstreamHeaders removes disallowed proxy headers.
func SanitizeUpstreamHeaders(raw map[string]string) map[string]string {
	disallowed := map[string]bool{
		"host":              true,
		"cf-connecting-ip":  true,
		"cf-ray":            true,
		"x-forwarded-for":   true,
		"x-forwarded-proto": true,
		"x-real-ip":         true,
		"connection":        true,
		"upgrade":           true,
	}

	out := make(map[string]string)
	for k, v := range raw {
		lk := strings.ToLower(strings.TrimSpace(k))
		if !disallowed[lk] && v != "" {
			out[k] = v
		}
	}
	return out
}

// AddCORSHeaders sets open CORS headers for streaming video playback.
func AddCORSHeaders(w http.ResponseWriter) {
	h := w.Header()
	h.Set("Access-Control-Allow-Origin", "*")
	h.Set("Access-Control-Allow-Methods", "GET, HEAD, POST, OPTIONS")
	h.Set("Access-Control-Allow-Headers", "Range, Authorization, Content-Type, X-Worker-Key, X-Proxy-Key, X-Requested-With")
	h.Set("Access-Control-Expose-Headers", "Content-Range, Content-Length, Accept-Ranges, Content-Type")
	h.Set("Access-Control-Max-Age", "86400")
}

// Base64URLEncode encodes bytes to URL-safe base64 without padding.
func Base64URLEncode(b []byte) string {
	return base64.RawURLEncoding.EncodeToString(b)
}

// Base64URLDecode decodes URL-safe base64 with or without padding.
func Base64URLDecode(s string) ([]byte, error) {
	return base64.RawURLEncoding.DecodeString(s)
}
