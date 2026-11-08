package proxy

import (
	"fmt"
	"net/url"
	"path"
	"regexp"
	"strings"
)

var (
	reAttrURI = regexp.MustCompile(`(?i)(URI\s*=\s*["'])([^"']+)(["'])`)
)

// EncodeResourceToken encodes an upstream URL to a URL-safe token.
func EncodeResourceToken(upstreamURL string) string {
	return Base64URLEncode([]byte(upstreamURL))
}

// DecodeResourceToken decodes a resource token back to the upstream URL.
func DecodeResourceToken(token string) (string, error) {
	bytes, err := Base64URLDecode(token)
	if err != nil {
		return "", err
	}
	return string(bytes), nil
}

// ResolveURI resolves relative or protocol-relative URI against base URL.
func ResolveURI(relativeURI, baseURL string) string {
	base, err := url.Parse(baseURL)
	if err != nil {
		return relativeURI
	}
	rel, err := url.Parse(relativeURI)
	if err != nil {
		return relativeURI
	}
	return base.ResolveReference(rel).String()
}

// RewritePlaylist rewrites HLS playlist m3u8 URLs through the proxy.
func RewritePlaylist(content, baseURL, sessionID, proxyBaseURL string) string {
	cleanProxyBase := strings.TrimRight(proxyBaseURL, "/")
	sessionPrefix := fmt.Sprintf("%s/session/%s", cleanProxyBase, sessionID)

	lines := strings.Split(content, "\n")
	var rewritten []string

	for _, line := range lines {
		trimmed := strings.TrimSpace(line)
		if trimmed == "" {
			rewritten = append(rewritten, line)
			continue
		}

		// 1. Rewrite Key tags: #EXT-X-KEY:...,URI="..."
		if strings.HasPrefix(trimmed, "#EXT-X-KEY:") {
			rewritten = append(rewritten, rewriteTagAttribute(line, baseURL, sessionPrefix, "key.key"))
			continue
		}

		// 2. Rewrite Audio & Subtitle Media tags: #EXT-X-MEDIA:...,URI="..."
		if strings.HasPrefix(trimmed, "#EXT-X-MEDIA:") {
			rewritten = append(rewritten, rewriteTagAttribute(line, baseURL, sessionPrefix, "media.m3u8"))
			continue
		}

		// 3. Rewrite Map tags: #EXT-X-MAP:URI="..."
		if strings.HasPrefix(trimmed, "#EXT-X-MAP:") {
			rewritten = append(rewritten, rewriteTagAttribute(line, baseURL, sessionPrefix, "init.mp4"))
			continue
		}

		// 4. Pass through all other directives unchanged
		if strings.HasPrefix(trimmed, "#") {
			rewritten = append(rewritten, line)
			continue
		}

		// 5. Line is a media segment or variant sub-playlist
		resolvedURL := ResolveURI(trimmed, baseURL)
		token := EncodeResourceToken(resolvedURL)
		filename := extractFilename(resolvedURL)
		if filename == "" {
			filename = "segment.ts"
		}
		proxiedURI := fmt.Sprintf("%s/res/%s/%s", sessionPrefix, token, filename)
		rewritten = append(rewritten, proxiedURI)
	}

	return strings.Join(rewritten, "\n")
}

func rewriteTagAttribute(line, baseURL, sessionPrefix, defaultFilename string) string {
	return reAttrURI.ReplaceAllStringFunc(line, func(match string) string {
		sub := reAttrURI.FindStringSubmatch(match)
		if len(sub) == 4 {
			prefix := sub[1]
			rawURI := sub[2]
			suffix := sub[3]

			resolved := ResolveURI(rawURI, baseURL)
			token := EncodeResourceToken(resolved)
			fn := extractFilename(resolved)
			if fn == "" {
				fn = defaultFilename
			}
			return fmt.Sprintf("%s%s/res/%s/%s%s", prefix, sessionPrefix, token, fn, suffix)
		}
		return match
	})
}

func extractFilename(urlStr string) string {
	u, err := url.Parse(urlStr)
	if err != nil {
		return ""
	}
	base := path.Base(u.Path)
	if base == "." || base == "/" {
		return ""
	}
	return strings.Split(base, "?")[0]
}
