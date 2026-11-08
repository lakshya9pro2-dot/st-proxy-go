package proxy

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"time"
)

const (
	DefaultSessionTTL = 4 * time.Hour
	DefaultSecretSalt = "vidara-netlify-proxy-default-salt-2026"
)

// SessionData holds the parsed playback session state.
type SessionData struct {
	URL       string            `json:"u"`
	Headers   map[string]string `json:"h"`
	VideoID   string            `json:"v,omitempty"`
	Type      string            `json:"t,omitempty"`
	Provider  string            `json:"p,omitempty"`
	CreatedAt int64             `json:"c"`
	ExpiresAt int64             `json:"e"`
	RandomID  string            `json:"rid,omitempty"`
}

// SessionManager manages in-memory cache and stateless cryptographic session tokens.
type SessionManager struct {
	mu        sync.RWMutex
	cache     map[string]*SessionData
	secretKey []byte
	ttl       time.Duration
}

// NewSessionManager creates a new SessionManager.
func NewSessionManager(secret string, ttl time.Duration) *SessionManager {
	if strings.TrimSpace(secret) == "" {
		secret = DefaultSecretSalt
	}
	if ttl <= 0 {
		ttl = DefaultSessionTTL
	}
	return &SessionManager{
		cache:     make(map[string]*SessionData),
		secretKey: []byte(secret),
		ttl:       ttl,
	}
}

// CreateSession generates a signed stateless session ID and stores in cache.
func (sm *SessionManager) CreateSession(targetURL string, headers map[string]string, videoID, mediaType, provider string) (string, int64, error) {
	now := time.Now().UnixMilli()
	expiresAt := now + sm.ttl.Milliseconds()

	randBytes := make([]byte, 16)
	if _, err := rand.Read(randBytes); err != nil {
		return "", 0, fmt.Errorf("failed to generate random ID: %w", err)
	}
	randHex := hex.EncodeToString(randBytes)

	data := &SessionData{
		URL:       targetURL,
		Headers:   headers,
		VideoID:   videoID,
		Type:      mediaType,
		Provider:  provider,
		CreatedAt: now,
		ExpiresAt: expiresAt,
		RandomID:  randHex,
	}

	payloadJSON, err := json.Marshal(data)
	if err != nil {
		return "", 0, fmt.Errorf("failed to marshal session payload: %w", err)
	}

	payloadEncoded := Base64URLEncode(payloadJSON)

	mac := hmac.New(sha256.New, sm.secretKey)
	mac.Write([]byte(payloadEncoded))
	sigBytes := mac.Sum(nil)
	sigEncoded := Base64URLEncode(sigBytes)

	sessionID := fmt.Sprintf("%s.%s.%s", randHex, payloadEncoded, sigEncoded)

	sm.mu.Lock()
	if len(sm.cache) > 10000 {
		sm.cache = make(map[string]*SessionData) // periodic prune
	}
	sm.cache[sessionID] = data
	sm.mu.Unlock()

	return sessionID, expiresAt, nil
}

// GetSession retrieves session by ID from cache or decodes the cryptographic signature.
func (sm *SessionManager) GetSession(sessionID string) (*SessionData, error) {
	if strings.TrimSpace(sessionID) == "" {
		return nil, fmt.Errorf("missing session ID")
	}

	// 1. Fast in-memory cache lookup
	sm.mu.RLock()
	cached, ok := sm.cache[sessionID]
	sm.mu.RUnlock()

	if ok && cached != nil {
		if time.Now().UnixMilli() <= cached.ExpiresAt {
			return cached, nil
		}
		sm.mu.Lock()
		delete(sm.cache, sessionID)
		sm.mu.Unlock()
		return nil, fmt.Errorf("session has expired")
	}

	// 2. Stateless cryptographic verification fallback
	parts := strings.Split(sessionID, ".")
	if len(parts) != 3 {
		return nil, fmt.Errorf("malformed session token")
	}

	payloadEncoded := parts[1]
	sigEncoded := parts[2]

	expectedMAC := hmac.New(sha256.New, sm.secretKey)
	expectedMAC.Write([]byte(payloadEncoded))
	expectedSig := expectedMAC.Sum(nil)

	actualSig, err := Base64URLDecode(sigEncoded)
	if err != nil || !hmac.Equal(expectedSig, actualSig) {
		return nil, fmt.Errorf("invalid session signature")
	}

	payloadBytes, err := Base64URLDecode(payloadEncoded)
	if err != nil {
		return nil, fmt.Errorf("failed to decode session payload: %w", err)
	}

	var data SessionData
	if err := json.Unmarshal(payloadBytes, &data); err != nil {
		return nil, fmt.Errorf("failed to unmarshal session data: %w", err)
	}

	if data.ExpiresAt > 0 && time.Now().UnixMilli() > data.ExpiresAt {
		return nil, fmt.Errorf("session has expired")
	}

	sm.mu.Lock()
	sm.cache[sessionID] = &data
	sm.mu.Unlock()

	return &data, nil
}
