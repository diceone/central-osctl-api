package main

import (
    "encoding/json"
    "fmt"
    "io"
    "log"
    "net/http"
    "net/url"
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
    clients map[string]OsctlClient
    mu      sync.Mutex
}

func NewCentralAPI() *CentralAPI {
    return &CentralAPI{
        clients: make(map[string]OsctlClient),
    }
}

func (api *CentralAPI) RegisterClient(w http.ResponseWriter, r *http.Request) {
    var client OsctlClient
    if err := json.NewDecoder(r.Body).Decode(&client); err != nil {
        http.Error(w, err.Error(), http.StatusBadRequest)
        return
    }
    api.mu.Lock()
    defer api.mu.Unlock()
    api.clients[client.ID] = client
    w.WriteHeader(http.StatusOK)
}

func (api *CentralAPI) UnregisterClient(w http.ResponseWriter, r *http.Request) {
    var client OsctlClient
    if err := json.NewDecoder(r.Body).Decode(&client); err != nil {
        http.Error(w, err.Error(), http.StatusBadRequest)
        return
    }
    api.mu.Lock()
    defer api.mu.Unlock()
    delete(api.clients, client.ID)
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
    proxyURL.RawQuery = r.URL.RawQuery

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
    api := NewCentralAPI()

    http.HandleFunc("/register", api.RegisterClient)
    http.HandleFunc("/unregister", api.UnregisterClient)
    http.HandleFunc("/clients", api.ListClients)
    http.HandleFunc("/proxy", api.ProxyRequest)

    fmt.Println("Central API server is running on port 12001")
    log.Fatal(http.ListenAndServe(":12001", nil))
}
