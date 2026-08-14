package reconcile

import (
	"context"
	"fmt"
	"net/http"
	"time"
)

// DefaultProbeInterval is how often a health probe retries.
const DefaultProbeInterval = 2 * time.Second

// Probe polls url until it answers 2xx or the timeout expires. A service that
// starts but never becomes healthy is a failed deploy, not a slow one.
func Probe(ctx context.Context, url string, timeout, interval time.Duration) error {
	if timeout <= 0 {
		timeout = manifestDefaultTimeout
	}
	if interval <= 0 {
		interval = DefaultProbeInterval
	}

	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	client := &http.Client{Timeout: interval + time.Second}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	// last holds the newest failure the endpoint itself produced. A failure
	// caused by the deadline expiring mid-request would otherwise overwrite the
	// real reason with "context deadline exceeded", which tells an operator
	// nothing about why the service is unhealthy.
	var last error
	for {
		err := probeOnce(ctx, client, url)
		if err == nil {
			return nil
		}
		if ctx.Err() == nil || last == nil {
			last = err
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("health probe %s did not pass within %s: %w", url, timeout, last)
		case <-ticker.C:
		}
	}
}

func probeOnce(ctx context.Context, client *http.Client, url string) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return fmt.Errorf("build probe request: %w", err)
	}
	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("probe request: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return fmt.Errorf("probe answered %d", resp.StatusCode)
	}
	return nil
}
