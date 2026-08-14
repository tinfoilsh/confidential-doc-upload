// healthcheck performs one bounded HTTP probe for the container runtime.
package main

import (
	"context"
	"flag"
	"fmt"
	"net"
	"net/http"
	"os"
	"time"
)

func main() {
	socketPath := flag.String("unix-socket", "", "dial this Unix socket instead of TCP")
	flag.Parse()
	if flag.NArg() != 1 {
		fatal("exactly one health URL is required")
	}

	dialer := &net.Dialer{Timeout: 2 * time.Second}
	transport := &http.Transport{
		DialContext:         dialer.DialContext,
		TLSHandshakeTimeout: 2 * time.Second,
	}
	if *socketPath != "" {
		transport.DialContext = func(ctx context.Context, _, _ string) (net.Conn, error) {
			return dialer.DialContext(ctx, "unix", *socketPath)
		}
	}
	client := &http.Client{
		Timeout:   3 * time.Second,
		Transport: transport,
		CheckRedirect: func(_ *http.Request, _ []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}

	response, err := client.Get(flag.Arg(0))
	if err != nil {
		fatal("probe failed: %v", err)
	}
	_ = response.Body.Close()
	if response.StatusCode != http.StatusOK {
		fatal("unexpected status %d", response.StatusCode)
	}
}

func fatal(format string, args ...any) {
	fmt.Fprintf(os.Stderr, "healthcheck: "+format+"\n", args...)
	os.Exit(1)
}
