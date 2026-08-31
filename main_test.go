package main

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"
	"time"
)

func newTestAPI(t *testing.T, apiKey string) (*CentralAPI, string) {
	t.Helper()
	file := joinTemp(t, "clients.json")
	return NewCentralAPI(file, apiKey), file
}

func joinTemp(t *testing.T, name string) string {
	t.Helper()
	return t.TempDir() + "/" + name
}

func mustRegister(t *testing.T, api *CentralAPI, c OsctlClient) {
	t.Helper()
	body, _ := json.Marshal(c)
	req := httptest.NewRequest(http.MethodPost, "/register", bytes.NewReader(body))
	if api.apiKey != "" {
		req.Header.Set("X-API-Key", api.apiKey)
	}
	rec := httptest.NewRecorder()
	api.RegisterClient(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("register %q failed: %d %s", c.ID, rec.Code, rec.Body.String())
	}
}

// --- registration ---

func TestRegisterRejectsWrongMethod(t *testing.T) {
	api, _ := newTestAPI(t, "")
	rec := httptest.NewRecorder()
	api.RegisterClient(rec, httptest.NewRequest(http.MethodGet, "/register", nil))
	if rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("got %d, want 405", rec.Code)
	}
	if !strings.Contains(rec.Header().Get("Allow"), http.MethodPost) {
		t.Fatalf("missing Allow header: %q", rec.Header().Get("Allow"))
	}
}

func TestRegisterRequiresAuth(t *testing.T) {
	api, _ := newTestAPI(t, "secret")
	rec := httptest.NewRecorder()
	api.RegisterClient(rec, httptest.NewRequest(http.MethodPost, "/register", strings.NewReader(`{"id":"a","api_url":"http://h"}`)))
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("no key: got %d, want 401", rec.Code)
	}
}

func TestRegisterRejectsInvalidInput(t *testing.T) {
	cases := []struct {
		name string
		body string
	}{
		{"missing id", `{"api_url":"http://h:1"}`},
		{"missing api_url", `{"id":"a"}`},
		{"empty host", `{"id":"a","api_url":"http://"}`},
		{"bad scheme", `{"id":"a","api_url":"ftp://h:1"}`},
		{"garbage", `{"id":"a","api_url":"::::"}`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			api, _ := newTestAPI(t, "")
			rec := httptest.NewRecorder()
			api.RegisterClient(rec, httptest.NewRequest(http.MethodPost, "/register", strings.NewReader(tc.body)))
			if rec.Code != http.StatusBadRequest {
				t.Fatalf("got %d, want 400 (%s)", rec.Code, rec.Body.String())
			}
		})
	}
}

func TestRegisterRejectsOversizedBody(t *testing.T) {
	api, _ := newTestAPI(t, "")
	big := strings.Repeat("a", maxRequestBodyBytes+1024)
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/register", strings.NewReader(`{"id":"a","api_url":"http://h","padding":"`+big+`"}`))
	api.RegisterClient(rec, req)
	if rec.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("got %d, want 413", rec.Code)
	}
}

func TestRegisterFailsWhenPersistenceFails(t *testing.T) {
	api, _ := newTestAPI(t, "")
	// Point persistence at a directory: rename onto a directory always fails.
	api.persistenceFile = t.TempDir()

	rec := httptest.NewRecorder()
	body := `{"id":"a","api_url":"http://h:1"}`
	api.RegisterClient(rec, httptest.NewRequest(http.MethodPost, "/register", strings.NewReader(body)))
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("got %d, want 500", rec.Code)
	}
	api.mu.Lock()
	_, exists := api.clients["a"]
	api.mu.Unlock()
	if exists {
		t.Fatal("client should have been rolled back after persist failure")
	}
}

// --- listing ---

func TestClientsRequiresAuthAndRedactsPasswords(t *testing.T) {
	api, _ := newTestAPI(t, "secret")
	mustRegister(t, api, OsctlClient{ID: "c1", ApiURL: "http://h:1", Username: "u", Password: "verysecret"})

	rec := httptest.NewRecorder()
	api.ListClients(rec, httptest.NewRequest(http.MethodGet, "/clients", nil))
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("no key: got %d, want 401", rec.Code)
	}

	req := httptest.NewRequest(http.MethodGet, "/clients", nil)
	req.Header.Set("X-API-Key", "secret")
	rec = httptest.NewRecorder()
	api.ListClients(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("with key: got %d, want 200", rec.Code)
	}
	if strings.Contains(rec.Body.String(), "verysecret") {
		t.Fatal("/clients leaked the password over the API")
	}
	var got map[string]OsctlClient
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("bad JSON: %v", err)
	}
	if got["c1"].Password != "" || got["c1"].ApiURL == "" || got["c1"].Username != "u" {
		t.Fatalf("unexpected redacted client: %+v", got["c1"])
	}
}

func TestClientsRejectsWrongMethod(t *testing.T) {
	api, _ := newTestAPI(t, "")
	rec := httptest.NewRecorder()
	api.ListClients(rec, httptest.NewRequest(http.MethodPost, "/clients", nil))
	if rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("got %d, want 405", rec.Code)
	}
}

// --- unregistration ---

func TestUnregisterSemantics(t *testing.T) {
	api, file := newTestAPI(t, "")
	mustRegister(t, api, OsctlClient{ID: "c1", ApiURL: "http://h:1"})

	// unknown id -> 404
	rec := httptest.NewRecorder()
	api.UnregisterClient(rec, httptest.NewRequest(http.MethodPost, "/unregister", strings.NewReader(`{"id":"nope"}`)))
	if rec.Code != http.StatusNotFound {
		t.Fatalf("unknown id: got %d, want 404", rec.Code)
	}
	// empty id -> 400
	rec = httptest.NewRecorder()
	api.UnregisterClient(rec, httptest.NewRequest(http.MethodPost, "/unregister", strings.NewReader(`{}`)))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("empty id: got %d, want 400", rec.Code)
	}
	// existing -> 200, and persists
	rec = httptest.NewRecorder()
	api.UnregisterClient(rec, httptest.NewRequest(http.MethodPost, "/unregister", strings.NewReader(`{"id":"c1"}`)))
	if rec.Code != http.StatusOK {
		t.Fatalf("existing id: got %d, want 200", rec.Code)
	}

	data, err := os.ReadFile(file)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(data), "c1") {
		t.Fatal("client should have been removed from persistence file")
	}
	_, err = os.Stat(file + ".tmp")
	if !os.IsNotExist(err) {
		t.Fatal("temp file should not remain after save")
	}
}

// --- proxying ---

type downstreamCapture struct {
	mu       sync.Mutex
	method   string
	path     string
	rawQuery string
	auth     string
	header   http.Header
	body     []byte
}

func startDownstream(t *testing.T, respBody string) (*httptest.Server, *downstreamCapture) {
	t.Helper()
	c := &downstreamCapture{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c.mu.Lock()
		c.method = r.Method
		c.path = r.URL.Path
		c.rawQuery = r.URL.RawQuery
		c.auth = r.Header.Get("Authorization")
		c.header = r.Header.Clone()
		c.body, _ = readAll(r)
		c.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(respBody))
	}))
	t.Cleanup(srv.Close)
	return srv, c
}

func readAll(r *http.Request) ([]byte, error) {
	var buf bytes.Buffer
	if _, err := buf.ReadFrom(r.Body); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

func TestProxyForwardsRequest(t *testing.T) {
	api, _ := newTestAPI(t, "")
	srv, c := startDownstream(t, "pong")
	mustRegister(t, api, OsctlClient{ID: "c1", ApiURL: srv.URL, Username: "admin", Password: "s3cret"})

	req := httptest.NewRequest(http.MethodPost, "/proxy?client_id=c1&path=/status&extra=1", strings.NewReader("payload"))
	req.Header.Set("Authorization", "Bearer caller-token") // must be overridden
	req.Header.Set("Connection", "close")                  // hop-by-hop, must be stripped
	req.Header.Set("X-Custom", "yes")
	rec := httptest.NewRecorder()
	api.ProxyRequest(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("got %d, want 200: %s", rec.Code, rec.Body.String())
	}
	if rec.Body.String() != "pong" {
		t.Fatalf("proxied body %q, want pong", rec.Body.String())
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.path != "/status" || c.rawQuery != "extra=1" {
		t.Fatalf("downstream got path=%q query=%q", c.path, c.rawQuery)
	}
	if c.method != http.MethodPost || string(c.body) != "payload" {
		t.Fatalf("downstream got method=%q body=%q", c.method, c.body)
	}
	want := "Basic " + base64.StdEncoding.EncodeToString([]byte("admin:s3cret"))
	if c.auth != want {
		t.Fatalf("downstream Authorization = %q, want %q", c.auth, want)
	}
	if c.header.Get("Connection") != "" {
		t.Fatal("hop-by-hop Connection header was forwarded")
	}
	if c.header.Get("X-Custom") != "yes" {
		t.Fatal("end-to-end header X-Custom was not forwarded")
	}
	if c.header.Get("Content-Length") != "7" {
		t.Fatalf("downstream Content-Length = %q, want 7 (chunked transfer)", c.header.Get("Content-Length"))
	}
}

func TestProxyMergesRegisteredQueryParams(t *testing.T) {
	api, _ := newTestAPI(t, "")
	srv, c := startDownstream(t, "ok")
	mustRegister(t, api, OsctlClient{ID: "c1", ApiURL: srv.URL + "/base?sort=asc&api=1"})

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/proxy?client_id=c1&path=/ram&sort=desc&extra=1", nil)
	api.ProxyRequest(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("got %d: %s", rec.Code, rec.Body.String())
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	q := c.rawQuery
	for _, want := range []string{"sort=desc", "extra=1", "api=1"} {
		if !strings.Contains(q, want) {
			t.Fatalf("query %q missing %q", q, want)
		}
	}
	if c.path != "/base/ram" {
		t.Fatalf("downstream path = %q, want /base/ram", c.path)
	}
}

func TestProxyRejectsPathTraversal(t *testing.T) {
	api, _ := newTestAPI(t, "")
	srv, _ := startDownstream(t, "ok")
	mustRegister(t, api, OsctlClient{ID: "c1", ApiURL: srv.URL})

	for _, p := range []string{"/../sibling", "/a/../../b", "/x/%2e%2e/y"} {
		rec := httptest.NewRecorder()
		api.ProxyRequest(rec, httptest.NewRequest(http.MethodGet, "/proxy?client_id=c1&path="+p, nil))
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("path %q: got %d, want 400", p, rec.Code)
		}
	}
}

func TestProxyValidationErrors(t *testing.T) {
	api, _ := newTestAPI(t, "")
	srv, _ := startDownstream(t, "ok")
	mustRegister(t, api, OsctlClient{ID: "c1", ApiURL: srv.URL})

	cases := []struct {
		name, url string
		want      int
	}{
		{"missing client_id", "/proxy?path=/", http.StatusBadRequest},
		{"missing path", "/proxy?client_id=c1", http.StatusBadRequest},
		{"unknown client", "/proxy?client_id=zz&path=/", http.StatusNotFound},
	}
	for _, tc := range cases {
		rec := httptest.NewRecorder()
		api.ProxyRequest(rec, httptest.NewRequest(http.MethodGet, tc.url, nil))
		if rec.Code != tc.want {
			t.Fatalf("%s: got %d, want %d", tc.name, rec.Code, tc.want)
		}
	}
}

func TestProxyRequiresAuth(t *testing.T) {
	api, _ := newTestAPI(t, "secret")
	rec := httptest.NewRecorder()
	api.ProxyRequest(rec, httptest.NewRequest(http.MethodGet, "/proxy?client_id=x&path=/", nil))
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("got %d, want 401", rec.Code)
	}
}

// --- persistence ---

func TestPersistenceRoundTripAndAtomicity(t *testing.T) {
	api, file := newTestAPI(t, "")
	mustRegister(t, api, OsctlClient{ID: "c1", ApiURL: "http://h:1", Password: "pw"})

	if _, err := os.Stat(file); err != nil {
		t.Fatalf("persistence file missing: %v", err)
	}
	if _, err := os.Stat(file + ".tmp"); !os.IsNotExist(err) {
		t.Fatal("temp file should not remain after save")
	}

	// A fresh instance must load the persisted client.
	reloaded := NewCentralAPI(file, "")
	reloaded.mu.Lock()
	got, ok := reloaded.clients["c1"]
	reloaded.mu.Unlock()
	if !ok || got.ApiURL != "http://h:1" || got.Password != "pw" {
		t.Fatalf("reload lost client: %+v", got)
	}
}

// ---- PATCH partial updates -------------------------------------------------

func TestPatchClientPartialUpdate(t *testing.T) {
	api, _ := newTestAPI(t, "")
	mustRegister(t, api, OsctlClient{ID: "c1", ApiURL: "http://127.0.0.1:1", Username: "u", Password: "p"})

	req := httptest.NewRequest(http.MethodPatch, "/register", strings.NewReader(`{"id":"c1","password":"new"}`))
	rec := httptest.NewRecorder()
	api.RegisterClient(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("PATCH failed: %d %s", rec.Code, rec.Body.String())
	}
	api.mu.Lock()
	c := api.clients["c1"]
	api.mu.Unlock()
	if c.Username != "u" || c.Password != "new" {
		t.Fatalf("unexpected merge: %+v", c)
	}
	if c.ApiURL != "http://127.0.0.1:1" {
		t.Fatal("api_url must be preserved on partial update")
	}
}

func TestPatchClientUnknownAndInvalid(t *testing.T) {
	api, _ := newTestAPI(t, "")
	req := httptest.NewRequest(http.MethodPatch, "/register", strings.NewReader(`{"id":"nope","password":"x"}`))
	rec := httptest.NewRecorder()
	api.RegisterClient(rec, req)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("want 404, got %d", rec.Code)
	}

	mustRegister(t, api, OsctlClient{ID: "c1", ApiURL: "http://127.0.0.1:1"})
	req = httptest.NewRequest(http.MethodPatch, "/register", strings.NewReader(`{"id":"c1","api_url":"notaurl"}`))
	rec = httptest.NewRecorder()
	api.RegisterClient(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("want 400, got %d", rec.Code)
	}
}

func TestPatchClearsTTL(t *testing.T) {
	api, _ := newTestAPI(t, "")
	mustRegister(t, api, OsctlClient{ID: "c1", ApiURL: "http://127.0.0.1:1", TtlSeconds: 60})
	req := httptest.NewRequest(http.MethodPatch, "/register", strings.NewReader(`{"id":"c1","ttl_seconds":0}`))
	rec := httptest.NewRecorder()
	api.RegisterClient(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d %s", rec.Code, rec.Body.String())
	}
	api.mu.Lock()
	c := api.clients["c1"]
	api.mu.Unlock()
	if c.ExpiresAt != nil {
		t.Fatal("TTL should be cleared")
	}
}

// ---- TTL expiry ------------------------------------------------------------

func TestTTLExpiry(t *testing.T) {
	api, _ := newTestAPI(t, "")
	mustRegister(t, api, OsctlClient{ID: "ttl", ApiURL: "http://127.0.0.1:1"})

	api.mu.Lock()
	c := api.clients["ttl"]
	past := time.Now().Add(-time.Minute)
	c.ExpiresAt = &past
	api.clients["ttl"] = c
	api.mu.Unlock()

	api.expireSweep()

	api.mu.Lock()
	_, exists := api.clients["ttl"]
	api.mu.Unlock()
	if exists {
		t.Fatal("expired client should be removed")
	}
	data, _ := os.ReadFile(api.persistenceFile)
	if strings.Contains(string(data), `"ttl"`) {
		t.Fatal("expired client should not be persisted")
	}
}

// ---- Tags ------------------------------------------------------------------

func TestTagFilter(t *testing.T) {
	api, _ := newTestAPI(t, "")
	mustRegister(t, api, OsctlClient{ID: "a", ApiURL: "http://127.0.0.1:1", Tags: []string{"prod", "eu"}})
	mustRegister(t, api, OsctlClient{ID: "b", ApiURL: "http://127.0.0.1:1", Tags: []string{"staging"}})

	req := httptest.NewRequest(http.MethodGet, "/clients?tag=prod&tag=eu", nil)
	rec := httptest.NewRecorder()
	api.ListClients(rec, req)
	var out map[string]json.RawMessage
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	if len(out) != 1 || out["a"] == nil {
		t.Fatalf("tag filter should return only a, got %v", out)
	}

	req = httptest.NewRequest(http.MethodGet, "/clients?tag=prod&tag=staging", nil)
	rec = httptest.NewRecorder()
	api.ListClients(rec, req)
	out = nil
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	if len(out) != 0 {
		t.Fatalf("AND semantics: no client has both tags, got %v", out)
	}
}

// ---- Rate limiting ---------------------------------------------------------

func TestRateLimit(t *testing.T) {
	api, _ := newTestAPI(t, "key1")
	api.rateLimitPerMinute = 2

	for i := 0; i < 2; i++ {
		req := httptest.NewRequest(http.MethodGet, "/clients", nil)
		req.Header.Set("X-API-Key", "key1")
		rec := httptest.NewRecorder()
		api.ListClients(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("request %d: want 200 got %d", i, rec.Code)
		}
	}
	req := httptest.NewRequest(http.MethodGet, "/clients", nil)
	req.Header.Set("X-API-Key", "key1")
	rec := httptest.NewRecorder()
	api.ListClients(rec, req)
	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("want 429, got %d", rec.Code)
	}
	if rec.Header().Get("Retry-After") == "" {
		t.Fatal("Retry-After header required")
	}
}

// ---- Multi-key auth --------------------------------------------------------

func TestMultiKeyPermissions(t *testing.T) {
	api, _ := newTestAPI(t, "")
	api.apiKeys = parseKeyList("full,readonly:ro")

	// read-only key can list
	req := httptest.NewRequest(http.MethodGet, "/clients", nil)
	req.Header.Set("X-API-Key", "readonly")
	rec := httptest.NewRecorder()
	api.ListClients(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("ro key list: want 200 got %d", rec.Code)
	}

	// read-only key cannot register
	req = httptest.NewRequest(http.MethodPost, "/register", strings.NewReader(`{"id":"x","api_url":"http://127.0.0.1:1"}`))
	req.Header.Set("X-API-Key", "readonly")
	rec = httptest.NewRecorder()
	api.RegisterClient(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("ro key register: want 403 got %d", rec.Code)
	}

	// full key can register
	req = httptest.NewRequest(http.MethodPost, "/register", strings.NewReader(`{"id":"x","api_url":"http://127.0.0.1:1"}`))
	req.Header.Set("X-API-Key", "full")
	rec = httptest.NewRecorder()
	api.RegisterClient(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("full key register: want 200 got %d", rec.Code)
	}

	// unknown key rejected
	req = httptest.NewRequest(http.MethodGet, "/clients", nil)
	req.Header.Set("X-API-Key", "bogus")
	rec = httptest.NewRecorder()
	api.ListClients(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("unknown key: want 401 got %d", rec.Code)
	}
}

// ---- Failover --------------------------------------------------------------

func TestFailoverToBackup(t *testing.T) {
	srv, c := startDownstream(t, `{"totalRamGb":16}`)
	api, _ := newTestAPI(t, "")
	mustRegister(t, api, OsctlClient{
		ID: "c1", ApiURL: "http://127.0.0.1:1", BackupURLs: []string{srv.URL},
		Username: "u", Password: "p",
	})

	req := httptest.NewRequest(http.MethodPost, "/proxy?client_id=c1&path=/ram", strings.NewReader(`{"x":1}`))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	api.ProxyRequest(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("proxy: %d %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "totalRamGb") {
		t.Fatalf("unexpected body: %q", rec.Body.String())
	}
	c.mu.Lock()
	gotBody, gotAuth := string(c.body), c.auth
	c.mu.Unlock()
	if gotBody != `{"x":1}` {
		t.Fatalf("body not replayed to backup: %q", gotBody)
	}
	wantAuth := "Basic " + base64.StdEncoding.EncodeToString([]byte("u:p"))
	if gotAuth != wantAuth {
		t.Fatalf("auth = %q, want client credentials", gotAuth)
	}
}

func TestNoFailoverForStreamingBody(t *testing.T) {
	srv, _ := startDownstream(t, "pong")
	api, _ := newTestAPI(t, "")
	mustRegister(t, api, OsctlClient{
		ID: "c1", ApiURL: "http://127.0.0.1:1", BackupURLs: []string{srv.URL},
	})

	// Unknown content length → cannot replay; backup must be skipped.
	req := httptest.NewRequest(http.MethodPost, "/proxy?client_id=c1&path=/status", strings.NewReader("payload"))
	req.ContentLength = -1
	rec := httptest.NewRecorder()
	api.ProxyRequest(rec, req)
	if rec.Code != http.StatusBadGateway {
		t.Fatalf("want 502, got %d", rec.Code)
	}
}

// ---- Circuit breaker -------------------------------------------------------

func TestCircuitBreaker(t *testing.T) {
	api, _ := newTestAPI(t, "")
	api.breakerThreshold = 2
	api.breakerCooldown = 50 * time.Millisecond
	mustRegister(t, api, OsctlClient{ID: "down", ApiURL: "http://127.0.0.1:1"})

	for i := 0; i < 2; i++ {
		req := httptest.NewRequest(http.MethodGet, "/proxy?client_id=down&path=/x", nil)
		rec := httptest.NewRecorder()
		api.ProxyRequest(rec, req)
		if want := http.StatusBadGateway; rec.Code != want {
			t.Fatalf("attempt %d: want %d got %d", i, want, rec.Code)
		}
	}

	req := httptest.NewRequest(http.MethodGet, "/proxy?client_id=down&path=/x", nil)
	rec := httptest.NewRecorder()
	api.ProxyRequest(rec, req)
	if want := http.StatusServiceUnavailable; rec.Code != want {
		t.Fatalf("breaker open: want %d got %d", want, rec.Code)
	}

	time.Sleep(80 * time.Millisecond)
	req = httptest.NewRequest(http.MethodGet, "/proxy?client_id=down&path=/x", nil)
	rec = httptest.NewRecorder()
	api.ProxyRequest(rec, req)
	if want := http.StatusBadGateway; rec.Code != want {
		t.Fatalf("half-open: want %d got %d", want, rec.Code)
	}
}

// ---- Health checks ---------------------------------------------------------

func TestHealthSweep(t *testing.T) {
	srv, _ := startDownstream(t, "ok")
	api, _ := newTestAPI(t, "")
	mustRegister(t, api, OsctlClient{ID: "c1", ApiURL: srv.URL})
	mustRegister(t, api, OsctlClient{ID: "c2", ApiURL: "http://127.0.0.1:1"})

	api.healthFailThreshold = 2
	api.healthSweep()

	api.mu.Lock()
	c1, c2 := api.clients["c1"], api.clients["c2"]
	api.mu.Unlock()
	if c1.Healthy == nil || !*c1.Healthy {
		t.Fatal("c1 should be healthy")
	}
	if c2.Healthy != nil && *c2.Healthy {
		t.Fatal("c2 should be unhealthy")
	}
	api.mu.Lock()
	failures := api.failures["c2"]
	api.mu.Unlock()
	if failures != 1 {
		t.Fatalf("c2 failures = %d, want 1", failures)
	}
}

func TestAutoDeregister(t *testing.T) {
	api, _ := newTestAPI(t, "")
	api.autoDeregister = true
	api.healthFailThreshold = 1
	mustRegister(t, api, OsctlClient{ID: "c2", ApiURL: "http://127.0.0.1:1"})

	api.healthSweep()

	api.mu.Lock()
	_, exists := api.clients["c2"]
	api.mu.Unlock()
	if exists {
		t.Fatal("failed client should be deregistered")
	}
	data, _ := os.ReadFile(api.persistenceFile)
	if strings.Contains(string(data), `"c2"`) {
		t.Fatal("deregistered client should not be persisted")
	}
}

func TestHealthOnRegister(t *testing.T) {
	srv, _ := startDownstream(t, "ok")
	api, _ := newTestAPI(t, "")
	api.healthOnRegister = true

	req := httptest.NewRequest(http.MethodPost, "/register", strings.NewReader(`{"id":"good","api_url":"`+srv.URL+`"}`))
	rec := httptest.NewRecorder()
	api.RegisterClient(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("healthy register: %d %s", rec.Code, rec.Body.String())
	}

	req = httptest.NewRequest(http.MethodPost, "/register", strings.NewReader(`{"id":"bad","api_url":"http://127.0.0.1:1"}`))
	rec = httptest.NewRecorder()
	api.RegisterClient(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("unhealthy register: want 400 got %d", rec.Code)
	}

	api.mu.Lock()
	c := api.clients["good"]
	api.mu.Unlock()
	if c.Healthy == nil || !*c.Healthy {
		t.Fatal("registered client should be marked healthy")
	}
}

// ---- TLS / skip_verify -----------------------------------------------------

func TestSkipVerify(t *testing.T) {
	tlsSrv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte("secure pong"))
	}))
	t.Cleanup(tlsSrv.Close)

	api, _ := newTestAPI(t, "")
	mustRegister(t, api, OsctlClient{ID: "skip", ApiURL: tlsSrv.URL, SkipVerify: true})
	mustRegister(t, api, OsctlClient{ID: "strict", ApiURL: tlsSrv.URL})

	req := httptest.NewRequest(http.MethodGet, "/proxy?client_id=skip&path=/x", nil)
	rec := httptest.NewRecorder()
	api.ProxyRequest(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("skip_verify proxy: %d", rec.Code)
	}

	req = httptest.NewRequest(http.MethodGet, "/proxy?client_id=strict&path=/x", nil)
	rec = httptest.NewRecorder()
	api.ProxyRequest(rec, req)
	if rec.Code != http.StatusBadGateway {
		t.Fatalf("verify against self-signed should be 502, got %d", rec.Code)
	}
}

// ---- Integration over the real mux ----------------------------------------

func newTestServer(t *testing.T, api *CentralAPI) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(api.routes())
	t.Cleanup(srv.Close)
	return srv
}

func TestIntegrationServer(t *testing.T) {
	srv, _ := startDownstream(t, `{"totalRamGb":16}`)
	api, _ := newTestAPI(t, "secret")
	ts := newTestServer(t, api)

	// register via HTTP API
	reqReg, err := http.NewRequest(http.MethodPost, ts.URL+"/register",
		strings.NewReader(`{"id":"c1","api_url":"`+srv.URL+`","username":"u","password":"p"}`))
	if err != nil {
		t.Fatal(err)
	}
	reqReg.Header.Set("Content-Type", "application/json")
	reqReg.Header.Set("X-API-Key", "secret")
	resp, err := http.DefaultClient.Do(reqReg)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("register: %d", resp.StatusCode)
	}

	// unauthenticated request rejected
	resp, err = http.Get(ts.URL + "/clients")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("unauth clients: want 401 got %d", resp.StatusCode)
	}

	authGet := func(path string) (int, map[string]any) {
		req, _ := http.NewRequest(http.MethodGet, ts.URL+path, nil)
		req.Header.Set("X-API-Key", "secret")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		var out map[string]any
		body, err := io.ReadAll(resp.Body)
		if err != nil {
			t.Fatal(err)
		}
		if len(body) > 0 && bytes.HasPrefix(bytes.TrimSpace(body), []byte("{")) {
			if err := json.Unmarshal(body, &out); err != nil {
				t.Fatalf("bad JSON %q: %v", string(body), err)
			}
		}
		return resp.StatusCode, out
	}

	// path-style proxy
	code, obj := authGet("/proxy/c1/ram?unit=gb")
	if code != http.StatusOK || obj["totalRamGb"] == nil {
		t.Fatalf("path proxy: %d %v", code, obj)
	}

	// legacy query-style proxy through the mux
	code, obj = authGet("/proxy?client_id=c1&path=/ram")
	if code != http.StatusOK || obj["totalRamGb"] == nil {
		t.Fatalf("legacy proxy: %d %v", code, obj)
	}

	// unknown client
	code, _ = authGet("/proxy/unknown/ram")
	if code != http.StatusNotFound {
		t.Fatalf("unknown client proxy: want 404 got %d", code)
	}

	// open endpoints
	resp, err = http.Get(ts.URL + "/healthz")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("healthz: %d", resp.StatusCode)
	}

	// protected endpoints
	code, _ = authGet("/metrics")
	if code != http.StatusOK {
		t.Fatalf("metrics: %d", code)
	}

	code, _ = authGet("/audit")
	if code != http.StatusOK {
		t.Fatalf("audit: %d", code)
	}
}

func TestVersionAndRequestID(t *testing.T) {
	api, _ := newTestAPI(t, "")
	ts := newTestServer(t, api)

	resp, err := http.Get(ts.URL + "/version")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("version: %d", resp.StatusCode)
	}
	if resp.Header.Get("X-Request-ID") == "" {
		t.Fatal("middleware should assign X-Request-ID")
	}
	var vobj map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&vobj); err != nil {
		t.Fatal(err)
	}
	if vobj["version"] == nil {
		t.Fatalf("version body: %v", vobj)
	}

	req, _ := http.NewRequest(http.MethodGet, ts.URL+"/version", nil)
	req.Header.Set("X-Request-ID", "custom-id-1")
	resp2, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp2.Body.Close()
	if got := resp2.Header.Get("X-Request-ID"); got != "custom-id-1" {
		t.Fatalf("custom request id not echoed: %q", got)
	}
}

// ---- Live reload -----------------------------------------------------------

func TestMaybeReloadFile(t *testing.T) {
	api, file := newTestAPI(t, "")
	mustRegister(t, api, OsctlClient{ID: "old", ApiURL: "http://127.0.0.1:1"})

	// External modification must be picked up.
	ext := map[string]OsctlClient{
		"ext": {ID: "ext", ApiURL: "http://127.0.0.1:9", Username: "u", Password: "p"},
	}
	raw, _ := json.Marshal(ext)
	if err := os.WriteFile(file, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	if !api.maybeReloadFile() {
		t.Fatal("reload should trigger after external change")
	}
	api.mu.Lock()
	_, hasOld := api.clients["old"]
	_, hasNew := api.clients["ext"]
	api.mu.Unlock()
	if hasOld || !hasNew {
		t.Fatalf("expected ext-only after reload: old=%v new=%v", hasOld, hasNew)
	}

	// Second call without external change must not re-read.
	if api.maybeReloadFile() {
		t.Fatal("no reload expected without change")
	}
}
