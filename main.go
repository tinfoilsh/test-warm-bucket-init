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
	"os/exec"
	"strings"
	"sync"
	"time"
)

const (
	proxyPort   = "9000"
	sidecarPort = "9001"
)

// lifecycle follows the warm → active → killed pattern from
// code-execution-environment/api-server.
//
//   - warm:   sidecar not started. /__configure starts it and transitions
//             to active. All other requests return 503.
//   - active: sidecar running. Requests are reverse-proxied to :9001.
//             /__configure with the same creds is a no-op; with different
//             creds it restarts the sidecar.
//   - killed: sidecar stopped. Every request returns 410.
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
	mu      sync.Mutex
	state   lifecycle
	creds   credentials
	sidecar *exec.Cmd
}

var g = &gate{state: lifecycleWarm}

var sidecarProxy = httputil.NewSingleHostReverseProxy(
	&url.URL{Scheme: "http", Host: "localhost:" + sidecarPort},
)

func main() {
	mux := http.NewServeMux()
	mux.HandleFunc("/__configure", configureHandler)
	mux.HandleFunc("/__status", statusHandler)
	mux.HandleFunc("/__kill", killHandler)
	mux.HandleFunc("/health", healthHandler)
	mux.HandleFunc("/", proxyHandler)

	log.Printf("sidecar-init listening on :%s (warm)", proxyPort)
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

	// Different creds while active: kill old sidecar, start new one
	if g.state == lifecycleActive {
		killSidecar(g.sidecar)
	}

	cmd := buildSidecarCmd(creds)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	if err := cmd.Start(); err != nil {
		g.state = lifecycleWarm
		writeJSONError(w, http.StatusInternalServerError, "failed to start sidecar: "+err.Error())
		return
	}
	g.sidecar = cmd
	g.creds = creds

	if err := waitForSidecar(30 * time.Second); err != nil {
		killSidecar(cmd)
		g.state = lifecycleWarm
		writeJSONError(w, http.StatusInternalServerError, "sidecar failed to start: "+err.Error())
		return
	}

	g.state = lifecycleActive
	log.Println("sidecar started, transitioning to active")
	writeJSON(w, http.StatusOK, map[string]string{"status": "active"})

	// Reap the sidecar process in the background. If it exits unexpectedly
	// while we think it's active, transition back to warm so the caller
	// can reconfigure.
	go func() {
		err := cmd.Wait()
		log.Printf("sidecar exited: %v", err)
		g.mu.Lock()
		if g.state == lifecycleActive && g.sidecar == cmd {
			g.state = lifecycleWarm
			log.Println("sidecar exited unexpectedly, back to warm")
		}
		g.mu.Unlock()
	}()
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
	if g.state == lifecycleActive {
		killSidecar(g.sidecar)
	}
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
		sidecarProxy.ServeHTTP(w, r)
	}
}

func healthHandler(w http.ResponseWriter, r *http.Request) {
	// Proxy is alive regardless of sidecar state. cvmimage healthchecks
	// pass during warm (container is up, just not ready for traffic).
	w.WriteHeader(http.StatusOK)
}

func buildSidecarCmd(creds credentials) *exec.Cmd {
	cmd := exec.Command("java", "-jar", "/app/app.jar")
	// Inherit non-secret env from the container (AWS_REGION, MULTITENANT, etc.),
	// then inject the configured AWS creds + override PORT.
	env := filterEnv(os.Environ(), "AWS_ACCESS_KEY_ID", "AWS_SECRET_ACCESS_KEY", "AWS_SESSION_TOKEN", "PORT")
	env = append(env,
		"AWS_ACCESS_KEY_ID="+creds.AccessKeyID,
		"AWS_SECRET_ACCESS_KEY="+creds.SecretAccessKey,
		"PORT="+sidecarPort,
	)
	if creds.SessionToken != "" {
		env = append(env, "AWS_SESSION_TOKEN="+creds.SessionToken)
	}
	cmd.Env = env
	return cmd
}

func killSidecar(cmd *exec.Cmd) {
	if cmd == nil || cmd.Process == nil {
		return
	}
	if err := cmd.Process.Kill(); err != nil {
		log.Printf("error killing sidecar: %v", err)
	}
}

func waitForSidecar(timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	addr := "localhost:" + sidecarPort
	for time.Now().Before(deadline) {
		conn, err := net.DialTimeout("tcp", addr, 500*time.Millisecond)
		if err == nil {
			conn.Close()
			return nil
		}
		time.Sleep(200 * time.Millisecond)
	}
	return fmt.Errorf("sidecar did not start within %s", timeout)
}

func filterEnv(env []string, names ...string) []string {
	drop := make(map[string]bool, len(names))
	for _, n := range names {
		drop[n] = true
	}
	var result []string
	for _, e := range env {
		if idx := strings.IndexByte(e, '='); idx > 0 {
			if drop[e[:idx]] {
				continue
			}
		}
		result = append(result, e)
	}
	return result
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
