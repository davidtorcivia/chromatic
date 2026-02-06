package webrtc

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/pion/webrtc/v4"
)

const (
	cloudflareTURNEndpoint = "https://rtc.live.cloudflare.com/v1/turn/keys/%s/credentials/generate-ice-servers"
)

type cloudflareTURNProviderConfig struct {
	KeyID      string
	APIToken   string
	TTLSeconds int
	Skew       time.Duration
}

type cloudflareTURNProvider struct {
	client *http.Client
	cfg    cloudflareTURNProviderConfig

	mu         sync.Mutex
	cachedUser string
	cachedPass string
	expiresAt  time.Time
}

type cloudflareTURNGenerateRequest struct {
	TTL int `json:"ttl"`
}

type cloudflareTURNServer struct {
	URLs       []string `json:"urls"`
	Username   string   `json:"username"`
	Credential string   `json:"credential"`
}

type cloudflareTURNGenerateResponse struct {
	ICEServers []cloudflareTURNServer `json:"iceServers"`
}

type cloudflareTURNAPIError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

type cloudflareTURNWrappedResponse struct {
	Result cloudflareTURNGenerateResponse `json:"result"`
	Errors []cloudflareTURNAPIError       `json:"errors"`
}

func newCloudflareTURNProvider(cfg cloudflareTURNProviderConfig) *cloudflareTURNProvider {
	return &cloudflareTURNProvider{
		cfg: cfg,
		client: &http.Client{
			Timeout: 5 * time.Second,
		},
	}
}

func (p *cloudflareTURNProvider) GetICEServer(ctx context.Context, preferredURLs []string) (webrtc.ICEServer, error) {
	username, credential, err := p.getCredentials(ctx)
	if err != nil {
		return webrtc.ICEServer{}, err
	}

	urls := sanitizeCloudflareURLs(preferredURLs)
	if len(urls) == 0 {
		urls = []string{
			"turn:turn.cloudflare.com:3478?transport=udp",
			"turn:turn.cloudflare.com:3478?transport=tcp",
			"turns:turn.cloudflare.com:443?transport=tcp",
		}
	}

	return webrtc.ICEServer{
		URLs:       urls,
		Username:   username,
		Credential: credential,
	}, nil
}

func (p *cloudflareTURNProvider) getCredentials(ctx context.Context) (string, string, error) {
	p.mu.Lock()
	if p.cachedUser != "" && p.cachedPass != "" && time.Now().Before(p.expiresAt) {
		user := p.cachedUser
		pass := p.cachedPass
		p.mu.Unlock()
		return user, pass, nil
	}
	p.mu.Unlock()

	reqBody, err := json.Marshal(cloudflareTURNGenerateRequest{
		TTL: p.cfg.TTLSeconds,
	})
	if err != nil {
		return "", "", fmt.Errorf("failed to marshal Cloudflare TURN request: %w", err)
	}

	url := fmt.Sprintf(cloudflareTURNEndpoint, p.cfg.KeyID)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(reqBody))
	if err != nil {
		return "", "", fmt.Errorf("failed to create Cloudflare TURN request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+p.cfg.APIToken)
	req.Header.Set("Content-Type", "application/json")

	resp, err := p.client.Do(req)
	if err != nil {
		return "", "", fmt.Errorf("failed to call Cloudflare TURN API: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return "", "", fmt.Errorf("Cloudflare TURN API returned HTTP %d", resp.StatusCode)
	}
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", "", fmt.Errorf("failed to read Cloudflare TURN response: %w", err)
	}

	// This endpoint can return either direct payload or wrapped payload.
	var direct cloudflareTURNGenerateResponse
	if err := json.Unmarshal(body, &direct); err == nil && len(direct.ICEServers) > 0 {
		user, pass, err := firstCredentials(direct.ICEServers)
		if err != nil {
			return "", "", err
		}
		p.cacheCredentials(user, pass)
		return user, pass, nil
	}

	var wrapped cloudflareTURNWrappedResponse
	if err := json.Unmarshal(body, &wrapped); err != nil {
		return "", "", fmt.Errorf("failed to decode Cloudflare TURN response: %w", err)
	}

	if len(wrapped.Errors) > 0 {
		return "", "", fmt.Errorf("Cloudflare TURN API error: %s", wrapped.Errors[0].Message)
	}

	user, pass, err := firstCredentials(wrapped.Result.ICEServers)
	if err != nil {
		return "", "", err
	}

	p.cacheCredentials(user, pass)
	return user, pass, nil
}

func (p *cloudflareTURNProvider) cacheCredentials(username, credential string) {
	expiresAt := time.Now().Add(time.Duration(p.cfg.TTLSeconds)*time.Second - p.cfg.Skew)
	p.mu.Lock()
	p.cachedUser = username
	p.cachedPass = credential
	p.expiresAt = expiresAt
	p.mu.Unlock()
}

func firstCredentials(servers []cloudflareTURNServer) (string, string, error) {
	for _, server := range servers {
		if server.Username != "" && server.Credential != "" {
			return server.Username, server.Credential, nil
		}
	}
	return "", "", fmt.Errorf("Cloudflare TURN response did not include credentials")
}

func sanitizeCloudflareURLs(urls []string) []string {
	filtered := make([]string, 0, len(urls))
	for _, raw := range urls {
		value := strings.TrimSpace(raw)
		if value == "" {
			continue
		}
		// Cloudflare docs explicitly discourage port 53 for TURN in production.
		if strings.Contains(value, ":53") {
			continue
		}
		filtered = append(filtered, value)
	}
	return filtered
}
