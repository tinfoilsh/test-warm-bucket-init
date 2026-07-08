package main

import (
	"encoding/json"
	"fmt"
	"log"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"os"
	"sync"
	"time"
)

const (
	proxyPort     = "9000"
	bucketsAddr   = "buckets:9001" // docker DNS, container name "buckets"
	credsFilePath = "/run/init/creds.env"
)

// lifecycle follows the warm → active → killed pattern from
// code-execution-environment/api-server.
//
//   - warm:   creds not yet injected. /__configure writes creds and
//             waits for the buckets container to start. All other
//             requests return 503.
//   - active: buckets container is up. Requests are reverse-proxied.
//             /__configure with the same creds is a no-op; with different
//             creds it rewrites the file and the buckets container
//             restarts (via docker restart policy).
//   - killed: session ended. Every request returns 410.
type lifecycle int

const (
	lifecycleWarm lifecycle = iota
	lifecycleActive
	lifecycleKilled
)

func (l lifecycle) String() string {
	switch l {
	case lifecycleWarm:
		return "warm"
	case lifecycleActive:
		return "active"
	case lifecycleKilled:
		return "killed"
	}
	return "unknown"
}

type credentials struct {
	AccessKeyID     string `json:"access_key_id"`
	SecretAccessKey string `json:"secret_access_key"`
	SessionToken    string `json:"session_token,omitempty"`
}

type gate struct {
	mu    sync.Mutex
	state lifecycle
	creds credentials
}

var g = &gate{state: lifecycleWarm}

var bucketsProxy = httputil.NewSingleHostReverseProxy(
	&url.URL{Scheme: "http", Host: bucketsAddr},
)

func main() {
	os.MkdirAll("/run/init", 0o700)

	mux := http.NewServeMux()
	mux.HandleFunc("/__configure", configureHandler)
	mux.HandleFunc("/__status", statusHandler)
	mux.HandleFunc("/__kill", killHandler)
	mux.HandleFunc("/health", healthHandler)
	mux.HandleFunc("/", proxyHandler)

	log.Printf("init-proxy listening on :%s (warm)", proxyPort)
	log.Fatal(http.ListenAndServe(":"+proxyPort, mux))
}

func configureHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeJSONError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}

	var creds credentials
	if err := json.NewDecoder(r.Body).Decode(&creds); err != nil {
		writeJSONError(w, http.StatusBadRequest, "invalid JSON: "+err.Error())
		return
	}
	if creds.AccessKeyID == "" || creds.SecretAccessKey == "" {
		writeJSONError(w, http.StatusBadRequest, "access_key_id and secret_access_key are required")
		return
	}

	g.mu.Lock()
	defer g.mu.Unlock()

	if g.state == lifecycleKilled {
		writeJSONError(w, http.StatusGone, "container session ended")
		return
	}

	// Idempotent: same creds, already active
	if g.state == lifecycleActive && creds == g.creds {
		writeJSON(w, http.StatusOK, map[string]string{"status": "active", "message": "already configured"})
		return
	}

	// Write creds to the shared volume. The buckets container's entrypoint
	// is blocked waiting for this file to appear (first configure) or
	// watching for it to change (reconfigure).
	if err := writeCredsFile(creds); err != nil {
		writeJSONError(w, http.StatusInternalServerError, "failed to write creds: "+err.Error())
		return
	}

	// Wait for the buckets container to come up on its port.
	if err := waitForBuckets(60 * time.Second); err != nil {
		writeJSONError(w, http.StatusInternalServerError, "buckets container did not start: "+err.Error())
		return
	}

	g.creds = creds
	g.state = lifecycleActive
	log.Println("buckets container started, transitioning to active")
	writeJSON(w, http.StatusOK, map[string]string{"status": "active"})
}

func statusHandler(w http.ResponseWriter, r *http.Request) {
	g.mu.Lock()
	defer g.mu.Unlock()
	writeJSON(w, http.StatusOK, map[string]string{"status": g.state.String()})
}

func killHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeJSONError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.state == lifecycleKilled {
		writeJSON(w, http.StatusOK, map[string]string{"status": "killed", "message": "already killed"})
		return
	}
	// Remove the creds file so the buckets container can't restart.
	os.Remove(credsFilePath)
	g.state = lifecycleKilled
	log.Println("killed, transitioning to killed")
	writeJSON(w, http.StatusOK, map[string]string{"status": "killed"})
}

func proxyHandler(w http.ResponseWriter, r *http.Request) {
	g.mu.Lock()
	state := g.state
	g.mu.Unlock()

	switch state {
	case lifecycleWarm:
		writeJSONError(w, http.StatusServiceUnavailable, "not configured")
	case lifecycleKilled:
		writeJSONError(w, http.StatusGone, "container session ended")
	case lifecycleActive:
		bucketsProxy.ServeHTTP(w, r)
	}
}

func healthHandler(w http.ResponseWriter, r *http.Request) {
	// Proxy is alive regardless of buckets state. cvmimage healthchecks
	// pass during warm (container is up, just not ready for traffic).
	w.WriteHeader(http.StatusOK)
}

// writeCredsFile writes AWS creds as a shell-sourceable env file to the
// shared volume. The buckets container's entrypoint sources this before
// exec-ing java.
func writeCredsFile(creds credentials) error {
	content := fmt.Sprintf("AWS_ACCESS_KEY_ID=%s\nAWS_SECRET_ACCESS_KEY=%s\n",
		creds.AccessKeyID, creds.SecretAccessKey)
	if creds.SessionToken != "" {
		content += fmt.Sprintf("AWS_SESSION_TOKEN=%s\n", creds.SessionToken)
	}
	return os.WriteFile(credsFilePath, []byte(content), 0o600)
}

func waitForBuckets(timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		conn, err := net.DialTimeout("tcp", bucketsAddr, 500*time.Millisecond)
		if err == nil {
			conn.Close()
			return nil
		}
		time.Sleep(500 * time.Millisecond)
	}
	return fmt.Errorf("buckets container did not accept connections within %s", timeout)
}

func writeJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(body)
}

func writeJSONError(w http.ResponseWriter, status int, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	fmt.Fprintf(w, `{"error":%q}`, msg)
}
