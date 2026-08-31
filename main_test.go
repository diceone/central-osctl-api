package main

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"
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
