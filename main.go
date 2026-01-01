package main

import (
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"os"
	"strings"
	"sync"
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
	return os.WriteFile(api.persistenceFile, data, 0600)
}

func (api *CentralAPI) authenticate(r *http.Request) bool {
	if api.apiKey == "" {
		return true // No authentication configured
	}
	authHeader := r.Header.Get("X-API-Key")
	return authHeader == api.apiKey
}

func (api *CentralAPI) RegisterClient(w http.ResponseWriter, r *http.Request) {
	if !api.authenticate(r) {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	var client OsctlClient
	if err := json.NewDecoder(r.Body).Decode(&client); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
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
	if err != nil || (parsedURL.Scheme != "http" && parsedURL.Scheme != "https") {
		http.Error(w, "invalid api_url: must be a valid http or https URL", http.StatusBadRequest)
		return
	}
	api.mu.Lock()
	api.clients[client.ID] = client
	if err := api.saveClients(); err != nil {
		log.Printf("Warning: Failed to persist clients: %v", err)
	}
	api.mu.Unlock()
	w.WriteHeader(http.StatusOK)
}

func (api *CentralAPI) UnregisterClient(w http.ResponseWriter, r *http.Request) {
	if !api.authenticate(r) {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	var client OsctlClient
	if err := json.NewDecoder(r.Body).Decode(&client); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	api.mu.Lock()
	delete(api.clients, client.ID)
	if err := api.saveClients(); err != nil {
		log.Printf("Warning: Failed to persist clients: %v", err)
	}
	api.mu.Unlock()
	w.WriteHeader(http.StatusOK)
}

func (api *CentralAPI) ListClients(w http.ResponseWriter, r *http.Request) {
	api.mu.Lock()
	defer api.mu.Unlock()
	if err := json.NewEncoder(w).Encode(api.clients); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
}

func (api *CentralAPI) ProxyRequest(w http.ResponseWriter, r *http.Request) {
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

	proxyURL, err := url.Parse(client.ApiURL)
	if err != nil {
		http.Error(w, "invalid client API URL", http.StatusInternalServerError)
		return
	}
	proxyURL.Path = strings.TrimSuffix(proxyURL.Path, "/") + proxyPath
	// Filter out client_id and path from query parameters
	query := r.URL.Query()
	query.Del("client_id")
	query.Del("path")
	proxyURL.RawQuery = query.Encode()

	req, err := http.NewRequest(r.Method, proxyURL.String(), r.Body)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	req.SetBasicAuth(client.Username, client.Password)
	req.Header = r.Header

	clientHTTP := &http.Client{}
	resp, err := clientHTTP.Do(req)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	defer resp.Body.Close()

	for k, v := range resp.Header {
		w.Header()[k] = v
	}
	w.WriteHeader(resp.StatusCode)
	if _, err := io.Copy(w, resp.Body); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
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

	http.HandleFunc("/register", api.RegisterClient)
	http.HandleFunc("/unregister", api.UnregisterClient)
	http.HandleFunc("/clients", api.ListClients)
	http.HandleFunc("/proxy", api.ProxyRequest)

	port := os.Getenv("PORT")
	if port == "" {
		port = "12001"
	}
	fmt.Printf("Central API server is running on port %s\n", port)
	log.Fatal(http.ListenAndServe(":"+port, nil))
}
