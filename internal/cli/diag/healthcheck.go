package diag

import (
	"fmt"
	"net/http"
	"time"

	"github.com/shiroha-a/mk/internal/config"
)

// healthcheckTimeout is the maximum time healthcheck waits for the local
// /healthz endpoint to respond.
const healthcheckTimeout = 3 * time.Second

// Healthcheck implements "elythia healthcheck": a single GET against
// http://127.0.0.1:<port>/healthz, using the port from the same configuration
// the running server reads. It returns 0 when the server answers 200.
func Healthcheck(args []string) int { return healthcheck(defaultEnv(), args) }

// healthcheck is Healthcheck with its dependencies passed in.
//
// distroless image には wget/curl が無いので、healthcheck をこの binary
// 自身で完結させるための専用モード (#621)。
func healthcheck(e env, args []string) int {
	path, parse := configFlag("healthcheck", e.stderr)
	if code, ok := parse(args); !ok {
		return code
	}
	cfg, err := config.Load(*path)
	if err != nil {
		fmt.Fprintf(e.stderr, "healthcheck: failed to load config: %v\n", err)
		return 1
	}
	return runHealthcheck(e, cfg.Port)
}

// runHealthcheck performs the request against the given local port.
func runHealthcheck(e env, port int) int {
	url := fmt.Sprintf("http://127.0.0.1:%d/healthz", port)
	client := &http.Client{Timeout: healthcheckTimeout}
	resp, err := client.Get(url)
	if err != nil {
		fmt.Fprintf(e.stderr, "healthcheck: %v\n", err)
		return 1
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		fmt.Fprintf(e.stderr, "healthcheck: status %d\n", resp.StatusCode)
		return 1
	}
	return 0
}
