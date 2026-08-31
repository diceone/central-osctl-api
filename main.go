package main

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"os"
	"os/signal"
	"sort"
	"strconv"
	"strings"
	"sync"
	"syscall"
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
	upstreamHeaderWait = 30 * time.Second
	healthProbeTimeout = 5 * time.Second
	serverReadTimeout  = 30 * time.Second
	serverHeaderRead   = 10 * time.Second
	serverIdleTimeout  = 120 * time.Second

	defaultHealthCheckPath = "/status"
	defaultKeyFPLength     = 6 // bytes of SHA-256 used as a key fingerprint
	auditRingSize          = 100
)

type OsctlClient struct {
	ID         string     `json:"id"`
	ApiURL     string     `json:"api_url"`
	BackupURLs []string   `json:"backup_urls,omitempty"`
	Username   string     `json:"username"`
	Password   string     `json:"password"`
	Tags       []string   `json:"tags,omitempty"`
	SkipVerify bool       `json:"skip_verify,omitempty"`
	TtlSeconds int        `json:"ttl_seconds,omitempty"`
	ExpiresAt  *time.Time `json:"expires_at,omitempty"`
	Healthy    *bool      `json:"healthy,omitempty"`
}

// clientPatch is the partial-update payload for PATCH /register. Nil fields
// are left untouched.
type clientPatch struct {
	ID         *string   `json:"id"`
	ApiURL     *string   `json:"api_url"`
	BackupURLs *[]string `json:"backup_urls"`
	Username   *string   `json:"username"`
	Password   *string   `json:"password"`
	Tags       *[]string `json:"tags"`
	SkipVerify *bool     `json:"skip_verify"`
	TtlSeconds *int      `json:"ttl_seconds"`
}

// apiKeyEntry is one accepted management/proxy key. fingerprint is used for
// logs and the audit ring so raw keys never appear there.
type apiKeyEntry struct {
	secret      string
	fingerprint string
	readOnly    bool
}

type authResult struct {
	ok         bool
	limited    bool
	retryAfter time.Duration
	entry      apiKeyEntry
}

// circuitBreaker trips after consecutive upstream failures and cools down
// before letting traffic through again.
type circuitBreaker struct {
	consecutive int
	openUntil   time.Time
}

type rateWindow struct {
	start time.Time
	count int
}

type auditEvent struct {
	Time       time.Time `json:"time"`
	Action     string    `json:"action"`
	Key        string    `json:"key,omitempty"`
	Client     string    `json:"client,omitempty"`
	Path       string    `json:"path,omitempty"`
	Status     int       `json:"status"`
	DurationMS int64     `json:"duration_ms,omitempty"`
	RequestID  string    `json:"request_id,omitempty"`
}

type CentralAPI struct {
	clients map[string]OsctlClient
	mu      sync.Mutex

	persistenceFile string
	savedContent    []byte // content the server last wrote or loaded

	apiKey  string // legacy single key (full permissions)
	apiKeys []apiKeyEntry

	// Optional features; zero values disable them.
	rateLimitPerMinute  int
	healthCheckPath     string
	healthCheckInterval time.Duration
	healthOnRegister    bool
	healthFailThreshold int
	autoDeregister      bool
	fileReloadInterval  time.Duration
	breakerThreshold    int
	breakerCooldown     time.Duration

	failures map[string]int // consecutive health check failures
	breakers map[string]*circuitBreaker
	rate     map[string]*rateWindow
	audit    []auditEvent

	metrics *metricsRegistry
}

func NewCentralAPI(persistenceFile, apiKey string) *CentralAPI {
	initHTTPClients()
	api := &CentralAPI{
		clients:             make(map[string]OsctlClient),
		persistenceFile:     persistenceFile,
		apiKey:              apiKey,
		healthCheckPath:     defaultHealthCheckPath,
		healthFailThreshold: 3,
		breakerThreshold:    3,
		breakerCooldown:     30 * time.Second,
		failures:            make(map[string]int),
		breakers:            make(map[string]*circuitBreaker),
		rate:                make(map[string]*rateWindow),
		metrics:             newMetricsRegistry(),
	}
	api.loadClients()
	return api
}

// --- persistence ---

func (api *CentralAPI) loadClients() {
	if api.persistenceFile == "" {
		return
	}
	data, err := os.ReadFile(api.persistenceFile)
	if err != nil {
		if !os.IsNotExist(err) {
			logEvent("clients_load_failed", map[string]any{"error": err.Error()})
		}
		return
	}
	api.mu.Lock()
	defer api.mu.Unlock()
	if err := json.Unmarshal(data, &api.clients); err != nil {
		logEvent("clients_parse_failed", map[string]any{"error": err.Error()})
		return
	}
	api.savedContent = data
	logEvent("clients_loaded", map[string]any{"count": len(api.clients), "file": api.persistenceFile})
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
	api.savedContent = data
	return nil
}

// maybeReloadFile picks up external edits of the persistence file. Changes we
// wrote ourselves are recognized by content equality and ignored.
func (api *CentralAPI) maybeReloadFile() bool {
	if api.persistenceFile == "" {
		return false
	}
	data, err := os.ReadFile(api.persistenceFile)
	if err != nil {
		return false
	}
	api.mu.Lock()
	defer api.mu.Unlock()
	if api.savedContent != nil && string(data) == string(api.savedContent) {
		return false
	}
	var fresh map[string]OsctlClient
	if err := json.Unmarshal(data, &fresh); err != nil {
		logEvent("clients_reload_failed", map[string]any{"error": err.Error()})
		return false
	}
	api.clients = fresh
	api.savedContent = data
	logEvent("clients_reloaded", map[string]any{"count": len(fresh)})
	return true
}

// --- authentication & rate limiting ---

func keyFingerprint(key string) string {
	sum := sha256.Sum256([]byte(key))
	return hex.EncodeToString(sum[:defaultKeyFPLength])
}

// allKeys returns every configured key, legacy API_KEY first.
func (api *CentralAPI) allKeys() []apiKeyEntry {
	entries := make([]apiKeyEntry, 0, len(api.apiKeys)+1)
	if api.apiKey != "" {
		entries = append(entries, apiKeyEntry{secret: api.apiKey, fingerprint: keyFingerprint(api.apiKey)})
	}
	entries = append(entries, api.apiKeys...)
	return entries
}

func (api *CentralAPI) keysConfigured() bool {
	return api.apiKey != "" || len(api.apiKeys) > 0
}

// allow is a fixed-window rate limiter keyed by an arbitrary identifier.
func (api *CentralAPI) allow(key string) (bool, time.Duration) {
	if api.rateLimitPerMinute <= 0 {
		return true, 0
	}
	now := time.Now()
	w := api.rate[key]
	if w == nil || now.Sub(w.start) >= time.Minute {
		w = &rateWindow{start: now}
		api.rate[key] = w
	}
	w.count++
	if w.count > api.rateLimitPerMinute {
		return false, time.Minute - now.Sub(w.start)
	}
	return true, 0
}

// authenticate validates the X-API-Key header (if any key is configured) and
// applies rate limiting per key, or per client IP when no keys exist.
func (api *CentralAPI) authenticate(r *http.Request) authResult {
	api.mu.Lock()
	defer api.mu.Unlock()
	if !api.keysConfigured() {
		ok, retry := api.allow("ip:" + clientIP(r))
		return authResult{ok: true, limited: !ok, retryAfter: retry}
	}
	presented := r.Header.Get("X-API-Key")
	for _, entry := range api.allKeys() {
		if subtle.ConstantTimeCompare([]byte(entry.secret), []byte(presented)) == 1 {
			ok, retry := api.allow("key:" + entry.fingerprint)
			return authResult{ok: true, limited: !ok, retryAfter: retry, entry: entry}
		}
	}
	return authResult{}
}

// authorize authenticates and enforces the read/write permission split.
// It writes an error response and returns false when the request must stop.
func (api *CentralAPI) authorize(w http.ResponseWriter, r *http.Request, needWrite bool) (authResult, bool) {
	res := api.authenticate(r)
	if !res.ok {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return res, false
	}
	if res.limited {
		wait := int(math.Ceil(res.retryAfter.Seconds()))
		if wait < 1 {
			wait = 1
		}
		w.Header().Set("Retry-After", strconv.Itoa(wait))
		http.Error(w, "rate limit exceeded", http.StatusTooManyRequests)
		return res, false
	}
	if needWrite && api.keysConfigured() && res.entry.readOnly {
		http.Error(w, "read-only API key", http.StatusForbidden)
		return res, false
	}
	return res, true
}

// --- shared HTTP plumbing ---

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

// validateURL parses s and requires an absolute http(s) URL with a host.
func validateURL(raw string) (*url.URL, bool) {
	if raw == "" {
		return nil, false
	}
	parsed, err := url.Parse(raw)
	if err != nil || (parsed.Scheme != "http" && parsed.Scheme != "https") || parsed.Host == "" {
		return nil, false
	}
	return parsed, true
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

var (
	defaultHTTPClient  *http.Client
	insecureHTTPClient *http.Client
	clientsOnce        sync.Once
)

func initHTTPClients() {
	clientsOnce.Do(func() {
		base := &http.Transport{
			Proxy:                 http.ProxyFromEnvironment,
			DialContext:           (&net.Dialer{Timeout: 10 * time.Second, KeepAlive: 30 * time.Second}).DialContext,
			ForceAttemptHTTP2:     true,
			MaxIdleConns:          100,
			IdleConnTimeout:       90 * time.Second,
			TLSHandshakeTimeout:   10 * time.Second,
			ExpectContinueTimeout: time.Second,
			ResponseHeaderTimeout: upstreamHeaderWait,
		}
		insecure := base.Clone()
		insecure.TLSClientConfig.InsecureSkipVerify = true
		defaultHTTPClient = &http.Client{Transport: base}
		insecureHTTPClient = &http.Client{Transport: insecure}
	})
}

// upstreamClient returns a pooled client; the insecure variant honors a
// client's skip_verify flag for self-signed downstream TLS.
func (api *CentralAPI) upstreamClient(skipVerify bool) *http.Client {
	if skipVerify {
		return insecureHTTPClient
	}
	return defaultHTTPClient
}

// --- logging, request IDs, metrics, audit ---

func logEvent(event string, fields map[string]any) {
	entry := make(map[string]any, len(fields)+2)
	for k, v := range fields {
		entry[k] = v
	}
	entry["event"] = event
	entry["time"] = time.Now().UTC().Format(time.RFC3339Nano)
	data, err := json.Marshal(entry)
	if err != nil {
		return
	}
	os.Stderr.Write(append(data, '\n'))
}

func newRequestID() string {
	b := make([]byte, 8)
	if _, err := rand.Read(b); err != nil {
		return strconv.FormatInt(time.Now().UnixNano(), 16)
	}
	return hex.EncodeToString(b)
}

func clientIP(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}

type statusWriter struct {
	http.ResponseWriter
	status      int
	wroteHeader bool
}

func (s *statusWriter) WriteHeader(code int) {
	if !s.wroteHeader {
		s.status = code
		s.wroteHeader = true
	}
	s.ResponseWriter.WriteHeader(code)
}

func (s *statusWriter) Write(b []byte) (int, error) {
	if !s.wroteHeader {
		s.status = http.StatusOK
		s.wroteHeader = true
	}
	return s.ResponseWriter.Write(b)
}

// wrap adds request-ID propagation, metrics, and a JSON access log around a
// handler registered on the real mux. Handlers called directly (tests) skip
// this layer.
func (api *CentralAPI) wrap(endpoint string, h http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		rid := sanitizeRequestID(r.Header.Get("X-Request-ID"))
		if rid == "" {
			rid = newRequestID()
		}
		w.Header().Set("X-Request-ID", rid)
		r.Header.Set("X-Request-ID", rid)
		sw := &statusWriter{ResponseWriter: w, status: http.StatusOK}
		start := time.Now()
		h(sw, r)
		elapsed := time.Since(start)
		api.metrics.add("http_requests_total", 1, "endpoint", endpoint, "code", strconv.Itoa(sw.status))
		api.metrics.addDuration(endpoint, elapsed.Seconds())
		logEvent("http_request", map[string]any{
			"endpoint": endpoint, "method": r.Method, "path": r.URL.Path,
			"status": sw.status, "duration_ms": elapsed.Milliseconds(), "request_id": rid,
		})
	}
}

func sanitizeRequestID(id string) string {
	id = strings.TrimSpace(id)
	if id == "" || len(id) > 128 {
		return ""
	}
	for _, c := range id {
		if !(c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '-' || c == '_' || c == '.') {
			return ""
		}
	}
	return id
}

func (api *CentralAPI) pushAudit(a auditEvent) {
	api.mu.Lock()
	defer api.mu.Unlock()
	api.audit = append(api.audit, a)
	if len(api.audit) > auditRingSize {
		api.audit = api.audit[len(api.audit)-auditRingSize:]
	}
}

func (api *CentralAPI) auditFromWriter(w http.ResponseWriter, a auditEvent, elapsed time.Duration) {
	if sw, ok := w.(*statusWriter); ok {
		a.Status = sw.status
		a.DurationMS = elapsed.Milliseconds()
		api.pushAudit(a)
	}
}

// --- metrics (Prometheus text format, no dependencies) ---

type metricsRegistry struct {
	mu       sync.Mutex
	samples  map[string]float64
	families map[string]string
}

func newMetricsRegistry() *metricsRegistry {
	return &metricsRegistry{
		samples: make(map[string]float64),
		families: map[string]string{
			"http_request_duration_seconds": "summary",
		},
	}
}

func escapeLabelValue(v string) string {
	r := strings.NewReplacer(`\`, `\\`, "\n", `\n`, `"`, `\"`)
	return r.Replace(v)
}

// line renders the full sample line "name{labels}".
func metricLine(name string, labels ...string) string {
	if len(labels) == 0 {
		return name
	}
	pairs := make([][2]string, 0, len(labels)/2)
	for i := 0; i+1 < len(labels); i += 2 {
		pairs = append(pairs, [2]string{labels[i], labels[i+1]})
	}
	sort.Slice(pairs, func(i, j int) bool { return pairs[i][0] < pairs[j][0] })
	var b strings.Builder
	b.WriteString(name)
	b.WriteByte('{')
	for i, p := range pairs {
		if i > 0 {
			b.WriteByte(',')
		}
		b.WriteString(p[0] + `="` + escapeLabelValue(p[1]) + `"`)
	}
	b.WriteByte('}')
	return b.String()
}

func (m *metricsRegistry) add(name string, v float64, labels ...string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.samples[metricLine(name, labels...)] += v
	if _, ok := m.families[name]; !ok {
		m.families[name] = "counter"
	}
}

func (m *metricsRegistry) addDuration(endpoint string, seconds float64) {
	m.add("http_request_duration_seconds_sum", seconds, "endpoint", endpoint)
	m.add("http_request_duration_seconds_count", 1, "endpoint", endpoint)
	m.mu.Lock()
	m.families["http_request_duration_seconds"] = "summary"
	m.mu.Unlock()
}

func baseMetricName(line string) string {
	if i := strings.IndexByte(line, '{'); i >= 0 {
		return line[:i]
	}
	return line
}

func (m *metricsRegistry) render(w io.Writer) {
	m.mu.Lock()
	defer m.mu.Unlock()
	lines := make([]string, 0, len(m.samples))
	for line := range m.samples {
		lines = append(lines, line)
	}
	sort.Strings(lines)
	seen := make(map[string]bool, len(m.families))
	for _, line := range lines {
		family := baseMetricName(line)
		// _sum/_count children of a summary family are reported under the
		// parent family name.
		if parent := strings.TrimSuffix(strings.TrimSuffix(family, "_count"), "_sum"); parent != family && m.families[parent] == "summary" {
			family = parent
		}
		if !seen[family] {
			seen[family] = true
			typ := "counter"
			if t, ok := m.families[family]; ok {
				typ = t
			}
			fmt.Fprintf(w, "# TYPE %s %s\n", family, typ)
		}
		fmt.Fprintf(w, "%s %v\n", line, m.samples[line])
	}
}

// --- client expiry (TTL) ---

func clientExpiry(now time.Time, c OsctlClient) (time.Time, bool) {
	if c.ExpiresAt != nil {
		return *c.ExpiresAt, true
	}
	if c.TtlSeconds > 0 {
		return now.Add(time.Duration(c.TtlSeconds) * time.Second), true
	}
	return time.Time{}, false
}

func (api *CentralAPI) clientExpired(now time.Time, c OsctlClient) bool {
	exp, has := clientExpiry(now, c)
	return has && now.After(exp)
}

// expireSweep removes expired clients; used by the janitor and handy for tests.
func (api *CentralAPI) expireSweep() int {
	api.mu.Lock()
	defer api.mu.Unlock()
	now := time.Now()
	removed := 0
	for id, c := range api.clients {
		if api.clientExpired(now, c) {
			delete(api.clients, id)
			removed++
		}
	}
	if removed == 0 {
		return 0
	}
	if err := api.saveClients(); err != nil {
		logEvent("clients_persist_failed", map[string]any{"error": err.Error()})
	}
	return removed
}

// --- health checks ---

func (api *CentralAPI) checkClientHealth(c OsctlClient) bool {
	base, ok := validateURL(c.ApiURL)
	if !ok {
		return false
	}
	checkURL := *base
	checkURL.Path = strings.TrimSuffix(checkURL.Path, "/") + api.healthCheckPath
	checkURL.RawQuery = ""
	ctx, cancel := context.WithTimeout(context.Background(), healthProbeTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, checkURL.String(), nil)
	if err != nil {
		return false
	}
	req.Header.Set("User-Agent", "central-osctl-api-healthcheck")
	if c.Username != "" || c.Password != "" {
		req.SetBasicAuth(c.Username, c.Password)
	}
	resp, err := api.upstreamClient(c.SkipVerify).Do(req)
	if err != nil {
		return false
	}
	defer resp.Body.Close()
	io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<16))
	return resp.StatusCode >= 200 && resp.StatusCode < 300
}

// healthSweep probes every registered client once and maintains the healthy
// flag, the failure counter, and (optionally) auto-deregistration.
func (api *CentralAPI) healthSweep() (healthy int, unhealthy int, deregistered int) {
	api.mu.Lock()
	snapshot := make([]OsctlClient, 0, len(api.clients))
	for _, c := range api.clients {
		snapshot = append(snapshot, c)
	}
	api.mu.Unlock()

	changed := false
	for _, c := range snapshot {
		expired := api.clientExpired(time.Now(), c)
		up := false
		if expired {
			// Expired clients are handled by expireSweep; skip the probe.
		} else if api.checkClientHealth(c) {
			up = false
		} else {
			up = true
		}
		api.mu.Lock()
		if up {
			api.failures[c.ID]++
		} else {
			api.failures[c.ID] = 0
		}
		current, exists := api.clients[c.ID]
		if exists {
			h := !up
			current.Healthy = &h
			api.clients[c.ID] = current
			changed = true
		}
		over := api.failures[c.ID] >= api.healthFailThreshold && api.healthFailThreshold > 0
		shouldDeregister := exists && api.autoDeregister && over && !expired
		api.mu.Unlock()
		if up {
			unhealthy++
		} else {
			healthy++
		}
		if shouldDeregister {
			api.mu.Lock()
			current, exists := api.clients[c.ID]
			if exists {
				delete(api.clients, c.ID)
				delete(api.failures, c.ID)
				if err := api.saveClients(); err != nil {
					api.clients[c.ID] = current
					logEvent("clients_persist_failed", map[string]any{"error": err.Error()})
				} else {
					deregistered++
					changed = false
					logEvent("client_auto_deregistered", map[string]any{"client": c.ID})
				}
			}
			api.mu.Unlock()
		}
	}
	if changed && deregistered == 0 {
		api.mu.Lock()
		if err := api.saveClients(); err != nil {
			logEvent("clients_persist_failed", map[string]any{"error": err.Error()})
		}
		api.mu.Unlock()
	}
	return healthy, unhealthy, deregistered
}

// --- circuit breaker ---

func (api *CentralAPI) breakerBlocked(id string) bool {
	api.mu.Lock()
	defer api.mu.Unlock()
	b := api.breakers[id]
	if b == nil {
		return false
	}
	if time.Now().Before(b.openUntil) {
		return true
	}
	// Cooldown elapsed: half-open; let one attempt through.
	return false
}

func (api *CentralAPI) breakerRecord(id string, success bool) {
	api.mu.Lock()
	defer api.mu.Unlock()
	b := api.breakers[id]
	if b == nil {
		b = &circuitBreaker{}
		api.breakers[id] = b
	}
	if success {
		b.consecutive = 0
		b.openUntil = time.Time{}
		return
	}
	b.consecutive++
	if api.breakerThreshold > 0 && b.consecutive >= api.breakerThreshold {
		b.openUntil = time.Now().Add(api.breakerCooldown)
	}
}

// --- registration ---

func (api *CentralAPI) RegisterClient(w http.ResponseWriter, r *http.Request) {
	start := time.Now()
	if !requireMethod(w, r, http.MethodPost, http.MethodPatch) {
		return
	}
	res, ok := api.authorize(w, r, true)
	if !ok {
		return
	}
	patch := r.Method == http.MethodPatch

	api.mu.Lock()
	client, action, errStatus, errMsg := api.buildClientPayload(w, r, patch)
	if errStatus != 0 {
		api.mu.Unlock()
		http.Error(w, errMsg, errStatus)
		return
	}
	changed, errStatus, errMsg := api.applyClient(client, patch)
	if errStatus != 0 {
		api.mu.Unlock()
		http.Error(w, errMsg, errStatus)
		return
	}
	api.mu.Unlock()

	api.auditFromWriter(w, auditEvent{
		Time: time.Now(), Action: action, Key: res.entry.fingerprint, Client: client.ID, RequestID: r.Header.Get("X-Request-ID"),
	}, time.Since(start))
	w.WriteHeader(http.StatusOK)
	_ = changed
}

// buildClientPayload decodes and validates a register (or patch) body. It
// must be called with api.mu held.
func (api *CentralAPI) buildClientPayload(w http.ResponseWriter, r *http.Request, patch bool) (client OsctlClient, action string, errStatus int, errMsg string) {
	if !patch {
		var c OsctlClient
		if !decodeJSONBody(w, r, &c) {
			return OsctlClient{}, "", http.StatusBadRequest, "invalid JSON body"
		}
		if c.ID == "" {
			return OsctlClient{}, "", http.StatusBadRequest, "client ID is required"
		}
		if _, ok := validateURL(c.ApiURL); !ok {
			return OsctlClient{}, "", http.StatusBadRequest, "invalid api_url: must be a valid http or https URL with a host"
		}
		for _, b := range c.BackupURLs {
			if _, ok := validateURL(b); !ok {
				return OsctlClient{}, "", http.StatusBadRequest, "invalid backup_urls entry: must be a valid http or https URL with a host"
			}
		}
		if c.TtlSeconds < 0 {
			return OsctlClient{}, "", http.StatusBadRequest, "ttl_seconds must not be negative"
		}
		if exp, has := clientExpiry(time.Now(), c); has {
			c.ExpiresAt = &exp
		}
		if api.healthOnRegister {
			if !api.checkClientHealth(c) {
				return OsctlClient{}, "", http.StatusBadRequest, "api_url is not reachable (health check failed)"
			}
			healthy := true
			c.Healthy = &healthy
		}
		return c, "register", 0, ""
	}

	var p clientPatch
	if !decodeJSONBody(w, r, &p) {
		return OsctlClient{}, "", http.StatusBadRequest, "invalid JSON body"
	}
	if p.ID == nil || *p.ID == "" {
		return OsctlClient{}, "", http.StatusBadRequest, "client ID is required"
	}
	existing, exists := api.clients[*p.ID]
	if !exists {
		return OsctlClient{}, "", http.StatusNotFound, "client not found"
	}
	client = existing
	if p.ApiURL != nil {
		if _, ok := validateURL(*p.ApiURL); !ok {
			return OsctlClient{}, "", http.StatusBadRequest, "invalid api_url: must be a valid http or https URL with a host"
		}
		client.ApiURL = *p.ApiURL
	}
	if p.BackupURLs != nil {
		for _, b := range *p.BackupURLs {
			if _, ok := validateURL(b); !ok {
				return OsctlClient{}, "", http.StatusBadRequest, "invalid backup_urls entry: must be a valid http or https URL with a host"
			}
		}
		client.BackupURLs = *p.BackupURLs
	}
	if p.Username != nil {
		client.Username = *p.Username
	}
	if p.Password != nil {
		client.Password = *p.Password
	}
	if p.Tags != nil {
		client.Tags = *p.Tags
	}
	if p.SkipVerify != nil {
		client.SkipVerify = *p.SkipVerify
	}
	if p.TtlSeconds != nil {
		if *p.TtlSeconds < 0 {
			return OsctlClient{}, "", http.StatusBadRequest, "ttl_seconds must not be negative"
		}
		client.TtlSeconds = *p.TtlSeconds
		if client.TtlSeconds == 0 {
			client.ExpiresAt = nil
		} else {
			exp := time.Now().Add(time.Duration(client.TtlSeconds) * time.Second)
			client.ExpiresAt = &exp
		}
	}
	return client, "patch", 0, ""
}

// applyClient validates reachability (opt-in), stores the client, and rolls
// back on persistence failure. api.mu must be held.
func (api *CentralAPI) applyClient(client OsctlClient, patch bool) (changed bool, errStatus int, errMsg string) {
	if !patch {
		prev, existed := api.clients[client.ID]
		api.clients[client.ID] = client
		if err := api.saveClients(); err != nil {
			if existed {
				api.clients[client.ID] = prev
			} else {
				delete(api.clients, client.ID)
			}
			logEvent("clients_persist_failed", map[string]any{"error": err.Error()})
			return false, http.StatusInternalServerError, "failed to persist client"
		}
		return true, 0, ""
	}
	prev := api.clients[client.ID]
	api.clients[client.ID] = client
	if err := api.saveClients(); err != nil {
		api.clients[client.ID] = prev
		logEvent("clients_persist_failed", map[string]any{"error": err.Error()})
		return false, http.StatusInternalServerError, "failed to persist client"
	}
	return true, 0, ""
}

func (api *CentralAPI) UnregisterClient(w http.ResponseWriter, r *http.Request) {
	start := time.Now()
	if !requireMethod(w, r, http.MethodPost) {
		return
	}
	res, ok := api.authorize(w, r, true)
	if !ok {
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
	delete(api.failures, client.ID)
	delete(api.breakers, client.ID)
	if err := api.saveClients(); err != nil {
		api.clients[client.ID] = removed
		api.mu.Unlock()
		logEvent("clients_persist_failed", map[string]any{"error": err.Error()})
		http.Error(w, "failed to persist client", http.StatusInternalServerError)
		return
	}
	api.mu.Unlock()

	api.auditFromWriter(w, auditEvent{
		Time: time.Now(), Action: "unregister", Key: res.entry.fingerprint, Client: client.ID, RequestID: r.Header.Get("X-Request-ID"),
	}, time.Since(start))
	w.WriteHeader(http.StatusOK)
}

func (api *CentralAPI) ListClients(w http.ResponseWriter, r *http.Request) {
	if !requireMethod(w, r, http.MethodGet) {
		return
	}
	if _, ok := api.authorize(w, r, false); !ok {
		return
	}
	wanted := r.URL.Query()["tag"]
	api.mu.Lock()
	data := make(map[string]OsctlClient, len(api.clients))
	now := time.Now()
	for id, c := range api.clients {
		if api.clientExpired(now, c) {
			continue
		}
		if len(wanted) > 0 && !hasAllTags(c, wanted) {
			continue
		}
		c.Password = "" // never expose downstream credentials
		data[id] = c
	}
	api.mu.Unlock()
	body, err := json.Marshal(data)
	if err != nil {
		logEvent("clients_encode_failed", map[string]any{"error": err.Error()})
		http.Error(w, "failed to encode clients", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(body)
}

func hasAllTags(c OsctlClient, wanted []string) bool {
	set := make(map[string]bool, len(c.Tags))
	for _, t := range c.Tags {
		set[t] = true
	}
	for _, w := range wanted {
		if !set[w] {
			return false
		}
	}
	return true
}

// --- background jobs ---

func (api *CentralAPI) runBackground(ctx context.Context) {
	expireTicker := time.NewTicker(30 * time.Second)
	defer expireTicker.Stop()
	var healthC, fileC <-chan time.Time
	if api.healthCheckInterval > 0 {
		t := time.NewTicker(api.healthCheckInterval)
		defer t.Stop()
		healthC = t.C
	}
	if api.fileReloadInterval > 0 {
		t := time.NewTicker(api.fileReloadInterval)
		defer t.Stop()
		fileC = t.C
	}
	for {
		select {
		case <-ctx.Done():
			return
		case <-expireTicker.C:
			if n := api.expireSweep(); n > 0 {
				logEvent("clients_expired", map[string]any{"count": n})
			}
		case <-healthC:
			healthy, unhealthy, dereg := api.healthSweep()
			if unhealthy > 0 || dereg > 0 {
				logEvent("health_sweep", map[string]any{"healthy": healthy, "unhealthy": unhealthy, "deregistered": dereg})
			}
		case <-fileC:
			api.maybeReloadFile()
		}
	}
}

// --- proxy ---

// proxyTarget is the resolved outcome of proxy routing parameters.
type proxyTarget struct {
	client  OsctlClient
	subPath string
}

// resolveProxy extracts client_id/path from either the legacy query-style
// request or the REST-style /proxy/{id}/*path route.
func (api *CentralAPI) resolveProxy(w http.ResponseWriter, r *http.Request) (proxyTarget, bool) {
	clientID := ""
	subPath := ""
	escPath := r.URL.EscapedPath()
	if r.URL.Path == "/proxy" {
		// Legacy query style: /proxy?client_id=X&path=/endpoint
		clientID = r.URL.Query().Get("client_id")
		if clientID == "" {
			http.Error(w, "client_id is required", http.StatusBadRequest)
			return proxyTarget{}, false
		}
		subPath = r.URL.Query().Get("path")
		if subPath == "" {
			http.Error(w, "path is required", http.StatusBadRequest)
			return proxyTarget{}, false
		}
	} else {
		// REST style: /proxy/{id}/...
		rest := strings.TrimPrefix(escPath, "/proxy/")
		idEsc, subEsc, _ := strings.Cut(rest, "/")
		id, err := url.PathUnescape(idEsc)
		if err != nil || id == "" {
			http.Error(w, "client_id is required", http.StatusBadRequest)
			return proxyTarget{}, false
		}
		clientID = id
		sub, err := url.PathUnescape("/" + strings.TrimSuffix(subEsc, "/"))
		if err != nil {
			http.Error(w, "invalid path", http.StatusBadRequest)
			return proxyTarget{}, false
		}
		subPath = sub
	}
	if hasDotDotSegment(subPath) {
		http.Error(w, "invalid path", http.StatusBadRequest)
		return proxyTarget{}, false
	}

	api.mu.Lock()
	client, exists := api.clients[clientID]
	expired := exists && api.clientExpired(time.Now(), client)
	api.mu.Unlock()
	if !exists || expired {
		http.Error(w, "client not found", http.StatusNotFound)
		return proxyTarget{}, false
	}
	return proxyTarget{client: client, subPath: subPath}, true
}

// ProxyRequest is the legacy query-parameter proxy:
// /proxy?client_id=X&path=/endpoint
func (api *CentralAPI) ProxyRequest(w http.ResponseWriter, r *http.Request) {
	api.proxyDispatch(w, r)
}

// ProxyPathRequest is the REST-style proxy: /proxy/{client_id}/endpoint
func (api *CentralAPI) ProxyPathRequest(w http.ResponseWriter, r *http.Request) {
	api.proxyDispatch(w, r)
}

func (api *CentralAPI) proxyDispatch(w http.ResponseWriter, r *http.Request) {
	start := time.Now()
	if !requireMethod(w, r, http.MethodGet, http.MethodPost, http.MethodPut, http.MethodPatch, http.MethodDelete, http.MethodHead) {
		return
	}
	res, ok := api.authorize(w, r, false)
	if !ok {
		return
	}
	target, ok := api.resolveProxy(w, r)
	if !ok {
		return
	}
	client := target.client
	subPath := target.subPath

	// Circuit breaker: fail fast while the client's upstreams are down.
	if api.breakerBlocked(client.ID) {
		http.Error(w, "client temporarily unavailable (circuit open)", http.StatusServiceUnavailable)
		return
	}

	// WebSocket/upgrade requests pass through ReverseProxy for proper
	// bidirectional streaming.
	if isUpgradeRequest(r) {
		status := api.proxyUpgrade(w, r, client, subPath)
		api.breakerRecord(client.ID, status >= 200 && status < 500)
		logEvent("proxy_upgrade", map[string]any{"client": client.ID, "status": status})
		return
	}

	// Prepare a replayable body when the size is known and small enough;
	// unknown or oversized bodies stream to the primary upstream only.
	var bodyData []byte
	canReplay := true
	switch {
	case r.Body == nil || r.ContentLength == 0:
	case r.ContentLength > 0 && r.ContentLength <= maxRequestBodyBytes:
		data, err := io.ReadAll(r.Body)
		if err != nil {
			http.Error(w, "failed to read request body", http.StatusBadRequest)
			return
		}
		bodyData = data
	default:
		canReplay = false
	}

	candidates := append([]string{client.ApiURL}, client.BackupURLs...)
	var resp *http.Response
	var lastErr error
	for i, rawBase := range candidates {
		base, perr := url.Parse(rawBase)
		if perr != nil || base == nil {
			logEvent("proxy_invalid_client_url", map[string]any{"client": client.ID, "url": rawBase})
			continue
		}
		if i > 0 && !canReplay {
			break
		}
		upstream := buildUpstreamURL(base, subPath, r.URL.Query())
		// The per-attempt context must stay alive until the response body has
		// been copied, so the cancel runs when the handler returns.
		attemptCtx, attemptCancel := context.WithTimeout(r.Context(), upstreamTimeout)
		defer attemptCancel()
		var body io.Reader
		if bodyData != nil {
			body = bytes.NewReader(bodyData)
		} else if i == 0 {
			body = r.Body
		} else {
			body = http.NoBody
		}
		req, err := http.NewRequestWithContext(attemptCtx, r.Method, upstream.String(), body)
		if err != nil {
			logEvent("proxy_build_failed", map[string]any{"client": client.ID, "error": err.Error()})
			continue
		}
		copyHeaders(req.Header, r.Header)
		if bodyData != nil {
			req.ContentLength = int64(len(bodyData))
		} else if r.ContentLength > 0 {
			req.ContentLength = r.ContentLength
		}
		// Set Basic Auth last so registered credentials win over anything the
		// caller sent.
		req.SetBasicAuth(client.Username, client.Password)
		resp, err = api.upstreamClient(client.SkipVerify).Do(req)
		if err != nil {
			lastErr = err
			api.metrics.add("proxy_upstream_errors_total", 1, "client", client.ID)
			continue
		}
		break
	}

	if resp == nil {
		api.breakerRecord(client.ID, false)
		if lastErr != nil {
			logEvent("proxy_upstream_failed", map[string]any{"client": client.ID, "error": lastErr.Error()})
		} else {
			logEvent("proxy_upstream_failed", map[string]any{"client": client.ID, "error": "no valid upstream"})
		}
		api.auditFromWriter(w, auditEvent{
			Time: time.Now(), Action: "proxy", Key: res.entry.fingerprint, Client: client.ID,
			Path: subPath, Status: http.StatusBadGateway, RequestID: r.Header.Get("X-Request-ID"),
		}, time.Since(start))
		http.Error(w, "upstream request failed", http.StatusBadGateway)
		return
	}
	defer resp.Body.Close()
	api.breakerRecord(client.ID, true)

	copyHeaders(w.Header(), resp.Header)
	w.WriteHeader(resp.StatusCode)
	if _, err := io.Copy(w, resp.Body); err != nil {
		// Response headers were already sent; there is nothing to recover.
		logEvent("proxy_copy_failed", map[string]any{"client": client.ID, "error": err.Error()})
	}
	api.metrics.add("proxy_requests_total", 1, "client", client.ID, "code", strconv.Itoa(resp.StatusCode))
	api.auditFromWriter(w, auditEvent{
		Time: time.Now(), Action: "proxy", Key: res.entry.fingerprint, Client: client.ID,
		Path: subPath, Status: resp.StatusCode, RequestID: r.Header.Get("X-Request-ID"),
	}, time.Since(start))
}

func isUpgradeRequest(r *http.Request) bool {
	return r.Header.Get("Upgrade") != "" && connectionWantsUpgrade(r.Header.Get("Connection"))
}

func connectionWantsUpgrade(h string) bool {
	for _, tok := range strings.Split(h, ",") {
		if strings.EqualFold(strings.TrimSpace(tok), "Upgrade") {
			return true
		}
	}
	return false
}

// buildUpstreamURL joins the registered base URL with the sub path and merges
// query parameters: parameters registered in api_url act as defaults, request
// parameters override them.
func buildUpstreamURL(base *url.URL, subPath string, reqQuery url.Values) *url.URL {
	u := *base
	u.Path = strings.TrimSuffix(u.Path, "/") + subPath
	merged := u.Query()
	for k, vs := range reqQuery {
		if k == "client_id" || k == "path" {
			continue
		}
		merged[k] = vs
	}
	u.RawQuery = merged.Encode()
	return &u
}

// proxyUpgrade handles WebSocket-style upgrade requests via ReverseProxy,
// which performs the 101 handshake swap transparently.
func (api *CentralAPI) proxyUpgrade(w http.ResponseWriter, r *http.Request, client OsctlClient, subPath string) int {
	base, ok := validateURL(client.ApiURL)
	if !ok {
		http.Error(w, "invalid client API URL", http.StatusInternalServerError)
		return http.StatusInternalServerError
	}
	var status = 0
	proxy := &httputil.ReverseProxy{
		Director: func(req *http.Request) {
			u := buildUpstreamURL(base, subPath, r.URL.Query())
			req.URL = u
			req.Host = u.Host
			// ReverseProxy copies inbound headers before the Director runs,
			// so setting auth here overrides anything the caller sent.
			req.SetBasicAuth(client.Username, client.Password)
		},
		Transport:     api.upstreamClient(client.SkipVerify).Transport,
		FlushInterval: -1, // required for upgrade/websocket streaming
		ErrorHandler: func(w http.ResponseWriter, r *http.Request, err error) {
			status = http.StatusBadGateway
			logEvent("proxy_upgrade_failed", map[string]any{"client": client.ID, "error": err.Error()})
			http.Error(w, "upstream request failed", http.StatusBadGateway)
		},
	}
	proxy.ServeHTTP(w, r)
	if status == 0 {
		status = http.StatusSwitchingProtocols
	}
	return status
}

// --- info endpoints ---

func (api *CentralAPI) VersionHandler(w http.ResponseWriter, r *http.Request) {
	if !requireMethod(w, r, http.MethodGet) {
		return
	}
	w.Header().Set("Content-Type", "application/json")
	body, _ := json.Marshal(map[string]string{
		"version": Version, "commit": Commit, "commit_date": CommitDate, "tree_state": TreeState,
	})
	_, _ = w.Write(body)
}

func (api *CentralAPI) HealthHandler(w http.ResponseWriter, r *http.Request) {
	if !requireMethod(w, r, http.MethodGet) {
		return
	}
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte("ok"))
}

func (api *CentralAPI) MetricsHandler(w http.ResponseWriter, r *http.Request) {
	if !requireMethod(w, r, http.MethodGet) {
		return
	}
	if _, ok := api.authorize(w, r, false); !ok {
		return
	}
	w.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")
	api.metrics.render(w)
}

func (api *CentralAPI) AuditHandler(w http.ResponseWriter, r *http.Request) {
	if !requireMethod(w, r, http.MethodGet) {
		return
	}
	if _, ok := api.authorize(w, r, false); !ok {
		return
	}
	api.mu.Lock()
	out := make([]auditEvent, len(api.audit))
	copy(out, api.audit)
	api.mu.Unlock()
	body, err := json.Marshal(out)
	if err != nil {
		http.Error(w, "failed to encode audit log", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_, _ = w.Write(body)
}

// routes builds the real HTTP mux with all endpoints wrapped for logging and
// metrics.
func (api *CentralAPI) routes() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/register", api.wrap("register", api.RegisterClient))
	mux.HandleFunc("/unregister", api.wrap("unregister", api.UnregisterClient))
	mux.HandleFunc("/clients", api.wrap("clients", api.ListClients))
	mux.HandleFunc("/proxy", api.wrap("proxy", api.ProxyRequest))
	mux.HandleFunc("POST /proxy/{clientID}", api.wrap("proxy_path", api.ProxyPathRequest))
	mux.HandleFunc("GET /proxy/{clientID}", api.wrap("proxy_path", api.ProxyPathRequest))
	mux.HandleFunc("HEAD /proxy/{clientID}", api.wrap("proxy_path", api.ProxyPathRequest))
	mux.HandleFunc("POST /proxy/{clientID}/", api.wrap("proxy_path", api.ProxyPathRequest))
	mux.HandleFunc("GET /proxy/{clientID}/", api.wrap("proxy_path", api.ProxyPathRequest))
	mux.HandleFunc("PUT /proxy/{clientID}/", api.wrap("proxy_path", api.ProxyPathRequest))
	mux.HandleFunc("PATCH /proxy/{clientID}/", api.wrap("proxy_path", api.ProxyPathRequest))
	mux.HandleFunc("DELETE /proxy/{clientID}/", api.wrap("proxy_path", api.ProxyPathRequest))
	mux.HandleFunc("HEAD /proxy/{clientID}/", api.wrap("proxy_path", api.ProxyPathRequest))
	mux.HandleFunc("/version", api.wrap("version", api.VersionHandler))
	mux.HandleFunc("/healthz", api.wrap("healthz", api.HealthHandler))
	mux.HandleFunc("/metrics", api.wrap("metrics", api.MetricsHandler))
	mux.HandleFunc("/audit", api.wrap("audit", api.AuditHandler))
	return mux
}

// --- environment helpers ---

func envStr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

func envInt(key string, def int) int {
	if v := os.Getenv(key); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			return n
		}
	}
	return def
}

func envBool(key string, def bool) bool {
	if v := strings.ToLower(os.Getenv(key)); v != "" {
		switch v {
		case "1", "true", "yes", "on":
			return true
		case "0", "false", "no", "off":
			return false
		}
	}
	return def
}

func envDuration(key string, def time.Duration) (time.Duration, error) {
	v := os.Getenv(key)
	if v == "" {
		return def, nil
	}
	d, err := time.ParseDuration(v)
	if err != nil {
		return 0, fmt.Errorf("invalid %s: %w", key, err)
	}
	if d < 0 {
		return 0, fmt.Errorf("%s must not be negative", key)
	}
	return d, nil
}

// parseKeyList parses API_KEYS entries: "key" or "key:ro".
func parseKeyList(csv string) []apiKeyEntry {
	csv = strings.TrimSpace(csv)
	if csv == "" {
		return nil
	}
	var out []apiKeyEntry
	for _, part := range strings.Split(csv, ",") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		readOnly := false
		if strings.HasSuffix(part, ":ro") {
			readOnly = true
			part = strings.TrimSuffix(part, ":ro")
		}
		if part == "" {
			continue
		}
		out = append(out, apiKeyEntry{secret: part, fingerprint: keyFingerprint(part), readOnly: readOnly})
	}
	return out
}

func main() {
	persistenceFile := envStr("PERSISTENCE_FILE", "clients.json")
	api := NewCentralAPI(persistenceFile, os.Getenv("API_KEY"))
	api.apiKeys = parseKeyList(os.Getenv("API_KEYS"))
	api.rateLimitPerMinute = envInt("RATE_LIMIT_PER_MINUTE", 0)
	api.healthCheckPath = envStr("HEALTH_CHECK_PATH", defaultHealthCheckPath)
	interval, err := envDuration("HEALTH_CHECK_INTERVAL", 0)
	if err != nil {
		logEvent("startup_failed", map[string]any{"error": err.Error()})
		os.Exit(1)
	}
	api.healthCheckInterval = interval
	api.healthOnRegister = envBool("HEALTH_CHECK_ON_REGISTER", false)
	api.healthFailThreshold = envInt("HEALTH_CHECK_THRESHOLD", 3)
	api.autoDeregister = envBool("AUTO_DEREGISTER", false)
	reloadInterval, err := envDuration("FILE_RELOAD_INTERVAL", 0)
	if err != nil {
		logEvent("startup_failed", map[string]any{"error": err.Error()})
		os.Exit(1)
	}
	api.fileReloadInterval = reloadInterval

	if !api.keysConfigured() {
		logEvent("auth_disabled", map[string]any{"warning": "No API_KEY or API_KEYS set - authentication disabled"})
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	go api.runBackground(ctx)

	port := envStr("PORT", "12001")
	srv := &http.Server{
		Addr:              ":" + port,
		Handler:           api.routes(),
		ReadTimeout:       serverReadTimeout,
		ReadHeaderTimeout: serverHeaderRead,
		WriteTimeout:      0, // long-lived proxied/streaming responses
		IdleTimeout:       serverIdleTimeout,
	}

	tlsCert := os.Getenv("HTTPS_CERT")
	tlsKey := os.Getenv("HTTPS_KEY")
	serveErr := make(chan error, 1)
	go func() {
		if tlsCert != "" && tlsKey != "" {
			logEvent("startup", map[string]any{"version": Version, "commit": Commit, "port": port, "tls": true})
			serveErr <- srv.ListenAndServeTLS(tlsCert, tlsKey)
		} else {
			logEvent("startup", map[string]any{"version": Version, "commit": Commit, "port": port, "tls": false})
			logEvent("listening", map[string]any{"port": port})
			serveErr <- srv.ListenAndServe()
		}
	}()

	select {
	case err := <-serveErr:
		if !errors.Is(err, http.ErrServerClosed) {
			logEvent("server_failed", map[string]any{"error": err.Error()})
			stop()
			os.Exit(1)
		}
	case <-ctx.Done():
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if err := srv.Shutdown(shutdownCtx); err != nil {
			logEvent("shutdown_failed", map[string]any{"error": err.Error()})
		} else {
			logEvent("shutdown_complete", nil)
		}
	}
}
