package main

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"os"
	"strconv"
	"time"
)

// runHealthcheck probes this container's own liveness endpoint and exits with a
// status the container runtime understands.
//
// It exists because the production image is distroless: there is no curl, no
// wget and no shell to write a health check with. Adding any of those purely to
// check the process is alive would put a general-purpose HTTP client and an
// interpreter into the runtime image, which is exactly what a minimal image is
// for avoiding. The binary already speaks HTTP, so it probes itself.
//
// It deliberately targets /livez rather than /readyz: a container that is alive
// but temporarily unable to reach the database should not be killed and
// restarted, it should be taken out of rotation. Readiness is the orchestrator's
// concern, and Railway is pointed at /readyz for exactly that.
func runHealthcheck() int {
	port := os.Getenv("PORT")
	if port == "" {
		port = "8080"
	}
	// Validate before building a URL from it. A non-numeric PORT would make
	// the probe fail confusingly, and validating keeps the URL free of any
	// value that did not come from a fixed numeric range.
	if number, err := strconv.Atoi(port); err != nil || number < 1 || number > 65535 {
		fmt.Fprintf(os.Stderr, "healthcheck: PORT must be a number between 1 and 65535\n")
		return 1
	}

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	// The host is the hard-coded loopback address; only the port is variable,
	// and it is validated above. There is no request-controlled input here, so
	// gosec's SSRF taint analysis is reporting a path that cannot exist.
	url := "http://" + net.JoinHostPort("127.0.0.1", port) + "/livez"
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil) //nolint:gosec // loopback only; port is validated above
	if err != nil {
		fmt.Fprintf(os.Stderr, "healthcheck: build request: %v\n", err)
		return 1
	}

	client := &http.Client{Timeout: 3 * time.Second}
	resp, err := client.Do(req) //nolint:gosec // loopback only; see above
	if err != nil {
		fmt.Fprintf(os.Stderr, "healthcheck: %v\n", err)
		return 1
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		fmt.Fprintf(os.Stderr, "healthcheck: %s returned %d\n", url, resp.StatusCode)
		return 1
	}
	return 0
}
