// Command healthcheck is a tiny probe used by the container HEALTHCHECK.
//
// It must not depend on the rest of the server, so it speaks HTTP directly
// instead of importing internal packages. ARCHITECTURE.md §29: healthz only
// reports process liveness and must not touch the database.
package main

import (
	"fmt"
	"net/http"
	"os"
	"time"
)

func main() {
	addr := os.Getenv("HEALTHCHECK_ADDR")
	if addr == "" {
		addr = "127.0.0.1:8080"
	}

	client := &http.Client{Timeout: 3 * time.Second}
	res, err := client.Get(fmt.Sprintf("http://%s/api/v1/healthz", addr))
	if err != nil {
		fmt.Fprintf(os.Stderr, "healthcheck: %v\n", err)
		os.Exit(1)
	}
	defer func() { _ = res.Body.Close() }()

	if res.StatusCode != http.StatusOK {
		fmt.Fprintf(os.Stderr, "healthcheck: status %d\n", res.StatusCode)
		os.Exit(1)
	}
}
