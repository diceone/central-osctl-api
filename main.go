package main

import (
	"crypto/subtle"
	"encoding/json"
	"errors"
	"io"
	"log"
	"net/http"
	"net/url"
	"os"
	"strings"
	"sync"
	"time"
)

// Set at build time by the SLSA releaser (see .slsa-goreleaser.yml) via
// -ldflags "-X main.Version=...".
var (
	Version    = "dev"
	Commit     = "unknown"
	CommitDate = "unknown"
	TreeState  = "unknown"
)

const (
	// maxRequestBodyBytes caps JSON bodies on the management endpoints.
	maxRequestBodyBytes = 1 << 20 // 1 MiB

	upstreamTimeout    = 30 * time.Second
	serverReadTimeout  = 30 * time.Second
	serverHeaderRead   = 10 * time.Second
	serverWriteTimeout = 60 * time.Second
	serverIdleTimeout  = 120 * time.Second
)

type OsctlClient struct {
	ID       string `json:"id"`
	ApiURL   string `json:"api_url"`
	Username string `json:"username"`
	Password string `json:"password"`
}

type CentralAPI struct {
	clients         map[string]OsctlClient
	mu              sync.Mutex
	persistenceFile string
	apiKey          string
}

func NewCentralAPI(persistenceFile, apiKey string) *CentralAPI {
	api := &CentralAPI{
		clients:         make(map[string]OsctlClient),
		persistenceFile: persistenceFile,
		apiKey:          apiKey,
	}
	api.loadClients()
	return api
}

func (api *CentralAPI) loadClients() {
	if api.persistenceFile == "" {
		return
	}
	data, err := os.ReadFile(api.persistenceFile)
	if err != nil {
		if !os.IsNotExist(err) {
			log.Printf("Warning: Failed to load clients: %v", err)
		}
		return
	}
	api.mu.Lock()
	defer api.mu.Unlock()
	if err := json.Unmarshal(data, &api.clients); err != nil {
		log.Printf("Warning: Failed to parse clients file: %v", err)
	} else {
		log.Printf("Loaded %d clients from %s", len(api.clients), api.persistenceFile)
	}
}

func (api *CentralAPI) saveClients() error {
	if api.persistenceFile == "" {
		return nil
	}
	data, err := json.MarshalIndent(api.clients, "", "  ")
	if err != nil {
		return err
	}
	// Write to a temp file and rename so a crash mid-write cannot corrupt the
	// persistence file.
	tmp := api.persistenceFile + ".tmp"
	if err := os.WriteFile(tmp, data, 0600); err != nil {
		return err
	}
	if err := os.Rename(tmp, api.persistenceFile); err != nil {
		os.Remove(tmp)
		return err
	}
	return nil
}

func (api *CentralAPI) authenticate(r *http.Request) bool {
	if api.apiKey == "" {
		return true // No authentication configured
	}
	authHeader := r.Header.Get("X-API-Key")
	return subtle.ConstantTimeCompare([]byte(authHeader), []byte(api.apiKey)) == 1
}

// requireMethod rejects requests whose HTTP method is not in the allowed set.
func requireMethod(w http.ResponseWriter, r *http.Request, methods ...string) bool {
	for _, m := range methods {
		if r.Method == m {
			return true
		}
	}
	w.Header().Set("Allow", strings.Join(methods, ", "))
	http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	return false
}

// decodeJSONBody decodes a JSON request body bounded by maxRequestBodyBytes.
func decodeJSONBody(w http.ResponseWriter, r *http.Request, v any) bool {
	r.Body = http.MaxBytesReader(w, r.Body, maxRequestBodyBytes)
	if err := json.NewDecoder(r.Body).Decode(v); err != nil {
		var maxErr *http.MaxBytesError
		if errors.As(err, &maxErr) {
			http.Error(w, "request body too large", http.StatusRequestEntityTooLarge)
		} else {
			http.Error(w, "invalid JSON body", http.StatusBadRequest)
		}
		return false
	}
	return true
}

// hopByHopHeaders must never be forwarded by a proxy (RFC 7230 §6.1).
var hopByHopHeaders = []string{
	"Connection",
	"Proxy-Connection",
	"Keep-Alive",
	"Proxy-Authenticate",
	"Proxy-Authorization",
	"Te",
	"Trailer",
	"Transfer-Encoding",
	"Upgrade",
}

func copyHeaders(dst, src http.Header) {
	for _, h := range hopByHopHeaders {
		src.Del(h)
	}
	for k, v := range src {
		dst[k] = v
	}
}

// proxyClient is shared so connections are pooled; the timeout guards against
// upstreams that never respond.
var proxyClient = &http.Client{Timeout: upstreamTimeout}

func (api *CentralAPI) RegisterClient(w http.ResponseWriter, r *http.Request) {
	if !requireMethod(w, r, http.MethodPost) {
		return
	}
	if !api.authenticate(r) {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	var client OsctlClient
	if !decodeJSONBody(w, r, &client) {
		return
	}
	// Validate client ID
	if client.ID == "" {
		http.Error(w, "client ID is required", http.StatusBadRequest)
		return
	}
	// Validate API URL
	if client.ApiURL == "" {
		http.Error(w, "api_url is required", http.StatusBadRequest)
		return
	}
	parsedURL, err := url.Parse(client.ApiURL)
	if err != nil || (parsedURL.Scheme != "http" && parsedURL.Scheme != "https") || parsedURL.Host == "" {
		http.Error(w, "invalid api_url: must be a valid http or https URL with a host", http.StatusBadRequest)
		return
	}
	api.mu.Lock()
	prev, existed := api.clients[client.ID]
	api.clients[client.ID] = client
	if err := api.saveClients(); err != nil {
		if existed {
			api.clients[client.ID] = prev
		} else {
			delete(api.clients, client.ID)
		}
		api.mu.Unlock()
		log.Printf("Failed to persist clients after registering %q: %v", client.ID, err)
		http.Error(w, "failed to persist client", http.StatusInternalServerError)
		return
	}
	api.mu.Unlock()
	w.WriteHeader(http.StatusOK)
}

func (api *CentralAPI) UnregisterClient(w http.ResponseWriter, r *http.Request) {
	if !requireMethod(w, r, http.MethodPost) {
		return
	}
	if !api.authenticate(r) {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	var client OsctlClient
	if !decodeJSONBody(w, r, &client) {
		return
	}
	if client.ID == "" {
		http.Error(w, "client ID is required", http.StatusBadRequest)
		return
	}
	api.mu.Lock()
	removed, existed := api.clients[client.ID]
	if !existed {
		api.mu.Unlock()
		http.Error(w, "client not found", http.StatusNotFound)
		return
	}
	delete(api.clients, client.ID)
	if err := api.saveClients(); err != nil {
		api.clients[client.ID] = removed
		api.mu.Unlock()
		log.Printf("Failed to persist clients after unregistering %q: %v", client.ID, err)
		http.Error(w, "failed to persist client", http.StatusInternalServerError)
		return
	}
	api.mu.Unlock()
	w.WriteHeader(http.StatusOK)
}

func (api *CentralAPI) ListClients(w http.ResponseWriter, r *http.Request) {
	if !requireMethod(w, r, http.MethodGet) {
		return
	}
	if !api.authenticate(r) {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	api.mu.Lock()
	data := make(map[string]OsctlClient, len(api.clients))
	for id, c := range api.clients {
		c.Password = "" // never expose downstream credentials
		data[id] = c
	}
	api.mu.Unlock()
	body, err := json.Marshal(data)
	if err != nil {
		log.Printf("Failed to encode clients: %v", err)
		http.Error(w, "failed to encode clients", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(body)
}

func (api *CentralAPI) ProxyRequest(w http.ResponseWriter, r *http.Request) {
	if !requireMethod(w, r, http.MethodGet, http.MethodPost, http.MethodPut, http.MethodPatch, http.MethodDelete, http.MethodHead) {
		return
	}
	if !api.authenticate(r) {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	clientID := r.URL.Query().Get("client_id")
	if clientID == "" {
		http.Error(w, "client_id is required", http.StatusBadRequest)
		return
	}

	api.mu.Lock()
	client, exists := api.clients[clientID]
	api.mu.Unlock()
	if !exists {
		http.Error(w, "client not found", http.StatusNotFound)
		return
	}

	proxyPath := r.URL.Query().Get("path")
	if proxyPath == "" {
		http.Error(w, "path is required", http.StatusBadRequest)
		return
	}
	if hasDotDotSegment(proxyPath) {
		http.Error(w, "invalid path", http.StatusBadRequest)
		return
	}

	proxyURL, err := url.Parse(client.ApiURL)
	if err != nil {
		log.Printf("Client %q has invalid API URL %q: %v", clientID, client.ApiURL, err)
		http.Error(w, "invalid client API URL", http.StatusInternalServerError)
		return
	}
	proxyURL.Path = strings.TrimSuffix(proxyURL.Path, "/") + proxyPath

	// Merge query parameters: parameters registered in api_url act as
	// defaults, request parameters override them. client_id and path are
	// routing parameters and are not forwarded.
	merged := proxyURL.Query()
	query := r.URL.Query()
	query.Del("client_id")
	query.Del("path")
	for k, vs := range query {
		merged[k] = vs
	}
	proxyURL.RawQuery = merged.Encode()

	req, err := http.NewRequestWithContext(r.Context(), r.Method, proxyURL.String(), r.Body)
	if err != nil {
		log.Printf("Failed to build upstream request for client %q: %v", clientID, err)
		http.Error(w, "failed to build upstream request", http.StatusInternalServerError)
		return
	}
	copyHeaders(req.Header, r.Header)
	if r.ContentLength > 0 {
		req.ContentLength = r.ContentLength
	}
	// Set Basic Auth last so registered credentials win over anything the
	// caller sent.
	req.SetBasicAuth(client.Username, client.Password)

	resp, err := proxyClient.Do(req)
	if err != nil {
		log.Printf("Proxy request to client %q failed: %v", clientID, err)
		http.Error(w, "upstream request failed", http.StatusBadGateway)
		return
	}
	defer resp.Body.Close()

	copyHeaders(w.Header(), resp.Header)
	w.WriteHeader(resp.StatusCode)
	if _, err := io.Copy(w, resp.Body); err != nil {
		// Response headers were already sent; there is nothing to recover.
		log.Printf("Failed to copy upstream response for client %q: %v", clientID, err)
	}
}

// hasDotDotSegment reports whether p contains a ".." path segment, which
// would let a caller escape the registered base path.
func hasDotDotSegment(p string) bool {
	for _, seg := range strings.Split(strings.TrimPrefix(p, "/"), "/") {
		if seg == ".." {
			return true
		}
	}
	return false
}

func main() {
	persistenceFile := os.Getenv("PERSISTENCE_FILE")
	if persistenceFile == "" {
		persistenceFile = "clients.json"
	}
	apiKey := os.Getenv("API_KEY")
	if apiKey != "" {
		log.Println("API Key authentication enabled")
	} else {
		log.Println("Warning: No API_KEY set - authentication disabled")
	}

	api := NewCentralAPI(persistenceFile, apiKey)

	mux := http.NewServeMux()
	mux.HandleFunc("/register", api.RegisterClient)
	mux.HandleFunc("/unregister", api.UnregisterClient)
	mux.HandleFunc("/clients", api.ListClients)
	mux.HandleFunc("/proxy", api.ProxyRequest)

	port := os.Getenv("PORT")
	if port == "" {
		port = "12001"
	}

	srv := &http.Server{
		Addr:              ":" + port,
		Handler:           mux,
		ReadTimeout:       serverReadTimeout,
		ReadHeaderTimeout: serverHeaderRead,
		WriteTimeout:      serverWriteTimeout,
		IdleTimeout:       serverIdleTimeout,
	}
	log.Printf("Central API server (version %s, commit %s) is running on port %s", Version, Commit, port)
	log.Fatal(srv.ListenAndServe())
}
