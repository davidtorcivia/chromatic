package webrtc

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"
)

func TestCloudflareTURNProvider_GetICEServer_DirectResponse_CachesCredentials(t *testing.T) {
	var calls int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&calls, 1)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"iceServers":[{"urls":["turn:turn.cloudflare.com:3478?transport=udp"],"username":"direct-user","credential":"direct-pass"}]}`))
	}))
	defer server.Close()

	provider := newCloudflareTURNProvider(cloudflareTURNProviderConfig{
		KeyID:            "test-key-id",
		APIToken:         "test-token",
		TTLSeconds:       3600,
		Skew:             60 * time.Second,
		EndpointTemplate: server.URL + "/v1/turn/keys/%s/credentials/generate-ice-servers",
	})

	preferred := []string{
		"turn:turn.cloudflare.com:53?transport=udp",
		"turn:turn.cloudflare.com:3478?transport=udp",
	}

	ice1, err := provider.GetICEServer(context.Background(), preferred)
	if err != nil {
		t.Fatalf("GetICEServer first call failed: %v", err)
	}
	ice2, err := provider.GetICEServer(context.Background(), preferred)
	if err != nil {
		t.Fatalf("GetICEServer second call failed: %v", err)
	}

	if atomic.LoadInt32(&calls) != 1 {
		t.Fatalf("expected 1 credential API call due to cache, got %d", calls)
	}

	if ice1.Username != "direct-user" || ice1.Credential != "direct-pass" {
		t.Fatalf("unexpected credentials: %+v", ice1)
	}
	if ice2.Username != "direct-user" || ice2.Credential != "direct-pass" {
		t.Fatalf("unexpected cached credentials: %+v", ice2)
	}
	if len(ice1.URLs) != 1 || ice1.URLs[0] != "turn:turn.cloudflare.com:3478?transport=udp" {
		t.Fatalf("expected sanitized preferred URL list, got %+v", ice1.URLs)
	}
}

func TestCloudflareTURNProvider_GetICEServer_WrappedResponse(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"result":{"iceServers":[{"urls":["turn:turn.cloudflare.com:3478?transport=tcp"],"username":"wrapped-user","credential":"wrapped-pass"}]},"errors":[]}`))
	}))
	defer server.Close()

	provider := newCloudflareTURNProvider(cloudflareTURNProviderConfig{
		KeyID:            "test-key-id",
		APIToken:         "test-token",
		TTLSeconds:       3600,
		Skew:             60 * time.Second,
		EndpointTemplate: server.URL + "/v1/turn/keys/%s/credentials/generate-ice-servers",
	})

	ice, err := provider.GetICEServer(context.Background(), nil)
	if err != nil {
		t.Fatalf("GetICEServer failed: %v", err)
	}

	if ice.Username != "wrapped-user" || ice.Credential != "wrapped-pass" {
		t.Fatalf("unexpected credentials from wrapped response: %+v", ice)
	}
	if len(ice.URLs) == 0 {
		t.Fatal("expected default URLs when preferred URL list is empty")
	}
}

func TestCloudflareTURNProvider_GetICEServer_CloudflareError(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"result":{"iceServers":[]},"errors":[{"code":1000,"message":"invalid token"}]}`))
	}))
	defer server.Close()

	provider := newCloudflareTURNProvider(cloudflareTURNProviderConfig{
		KeyID:            "test-key-id",
		APIToken:         "test-token",
		TTLSeconds:       3600,
		Skew:             60 * time.Second,
		EndpointTemplate: server.URL + "/v1/turn/keys/%s/credentials/generate-ice-servers",
	})

	if _, err := provider.GetICEServer(context.Background(), nil); err == nil {
		t.Fatal("expected error for wrapped Cloudflare API error payload")
	}
}

func TestCloudflareTURNProvider_RefreshesWhenCacheExpires(t *testing.T) {
	var calls int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		currentCall := atomic.AddInt32(&calls, 1)
		w.Header().Set("Content-Type", "application/json")
		if currentCall == 1 {
			_, _ = w.Write([]byte(`{"iceServers":[{"urls":["turn:turn.cloudflare.com:3478?transport=udp"],"username":"user-1","credential":"pass-1"}]}`))
			return
		}
		_, _ = w.Write([]byte(`{"iceServers":[{"urls":["turn:turn.cloudflare.com:3478?transport=udp"],"username":"user-2","credential":"pass-2"}]}`))
	}))
	defer server.Close()

	provider := newCloudflareTURNProvider(cloudflareTURNProviderConfig{
		KeyID:            "test-key-id",
		APIToken:         "test-token",
		TTLSeconds:       3600,
		Skew:             60 * time.Second,
		EndpointTemplate: server.URL + "/v1/turn/keys/%s/credentials/generate-ice-servers",
	})

	ice1, err := provider.GetICEServer(context.Background(), nil)
	if err != nil {
		t.Fatalf("first GetICEServer failed: %v", err)
	}

	// Force cache expiry without sleeping.
	provider.mu.Lock()
	provider.expiresAt = time.Now().Add(-time.Second)
	provider.mu.Unlock()

	ice2, err := provider.GetICEServer(context.Background(), nil)
	if err != nil {
		t.Fatalf("second GetICEServer failed: %v", err)
	}

	if atomic.LoadInt32(&calls) != 2 {
		t.Fatalf("expected 2 API calls after forced cache expiry, got %d", calls)
	}
	if ice1.Username == ice2.Username && ice1.Credential == ice2.Credential {
		t.Fatalf("expected refreshed credentials after cache expiry, got same values: %+v %+v", ice1, ice2)
	}
}
