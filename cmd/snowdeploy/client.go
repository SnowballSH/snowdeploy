package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"
)

// client talks to a snowdeployd. It holds no credential of its own: the token,
// if any, is read from a file the operator controls.
type client struct {
	baseURL   string
	tokenFile string
	http      *http.Client
}

func newClient(server, tokenFile string) (*client, error) {
	server = strings.TrimSuffix(strings.TrimSpace(server), "/")
	if server == "" {
		return nil, fmt.Errorf(
			"no server address: pass --server or set %s", envServer)
	}
	if _, err := url.Parse(server); err != nil {
		return nil, fmt.Errorf("server address %q: %w", server, err)
	}
	return &client{
		baseURL:   server,
		tokenFile: tokenFile,
		http:      &http.Client{Timeout: 30 * time.Second},
	}, nil
}

// authorize attaches the bearer token, if one is configured.
func (c *client) authorize(req *http.Request) error {
	if c.tokenFile == "" {
		return nil
	}
	raw, err := os.ReadFile(c.tokenFile) // #nosec G304 -- operator-supplied path
	if err != nil {
		return fmt.Errorf("read token file: %w", err)
	}
	token := strings.TrimSpace(string(raw))
	if token == "" {
		return fmt.Errorf("token file %s is empty", c.tokenFile)
	}
	req.Header.Set("Authorization", "Bearer "+token)
	return nil
}

func (c *client) newRequest(
	ctx context.Context, method, path string, body any,
) (*http.Request, error) {
	var reader io.Reader
	if body != nil {
		encoded, err := json.Marshal(body)
		if err != nil {
			return nil, fmt.Errorf("encode request: %w", err)
		}
		reader = bytes.NewReader(encoded)
	}

	// #nosec G704 -- the base URL is the operator's own --server value; pointing
	// the client at a chosen daemon is the entire purpose of this tool.
	req, err := http.NewRequestWithContext(ctx, method, c.baseURL+path, reader)
	if err != nil {
		return nil, fmt.Errorf("build request: %w", err)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if err := c.authorize(req); err != nil {
		return nil, err
	}
	return req, nil
}

// do performs a request and decodes a JSON response into out.
func (c *client) do(ctx context.Context, method, path string, body, out any) error {
	req, err := c.newRequest(ctx, method, path, body)
	if err != nil {
		return err
	}
	resp, err := c.http.Do(req) // #nosec G704 -- operator-chosen daemon address
	if err != nil {
		return fmt.Errorf("%s %s: %w", method, path, err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return fmt.Errorf("%s %s: %s", method, path, describe(resp))
	}
	if out == nil {
		return nil
	}
	if err := json.NewDecoder(resp.Body).Decode(out); err != nil {
		return fmt.Errorf("decode %s response: %w", path, err)
	}
	return nil
}

// describe turns an error response into something an operator can act on.
func describe(resp *http.Response) string {
	var payload struct {
		Error string `json:"error"`
	}
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 8192))
	if err := json.Unmarshal(body, &payload); err == nil && payload.Error != "" {
		return fmt.Sprintf("%s: %s", resp.Status, payload.Error)
	}
	trimmed := strings.TrimSpace(string(body))
	if trimmed == "" {
		return resp.Status
	}
	return fmt.Sprintf("%s: %s", resp.Status, trimmed)
}

// serviceStatus mirrors the API's service row.
type serviceStatus struct {
	Name              string              `json:"name"`
	Repository        string              `json:"repository"`
	ManifestDigest    string              `json:"manifestDigest"`
	RunningDigest     string              `json:"runningDigest"`
	LatestAvailable   string              `json:"latestAvailable"`
	Drifted           bool                `json:"drifted"`
	RegistryReachable bool                `json:"registryReachable"`
	LatestCheckedAt   time.Time           `json:"latestCheckedAt"`
	RepoWebURL        string              `json:"repoWebUrl"`
	LastDeploy        *historyEntry       `json:"lastDeploy"`
	Revisions         map[string]revision `json:"revisions"`
}

// revision mirrors what the API knows about the commit behind a digest.
type revision struct {
	SHA     string `json:"sha"`
	URL     string `json:"url"`
	Subject string `json:"subject"`
}

// historyEntry mirrors the API's journal entry.
type historyEntry struct {
	ID         int64     `json:"ID"`
	Service    string    `json:"Service"`
	Action     string    `json:"Action"`
	Actor      string    `json:"Actor"`
	OldDigest  string    `json:"OldDigest"`
	NewDigest  string    `json:"NewDigest"`
	PRNumber   int       `json:"PRNumber"`
	MergeSHA   string    `json:"MergeSHA"`
	State      string    `json:"State"`
	Detail     string    `json:"Detail"`
	StartedAt  time.Time `json:"StartedAt"`
	FinishedAt time.Time `json:"FinishedAt"`
}

// inFlight reports whether the entry describes a run still without a receipt.
func (e *historyEntry) inFlight() bool {
	return e != nil && e.FinishedAt.IsZero()
}

// event mirrors a streamed state transition.
type event struct {
	Service   string `json:"service"`
	Action    string `json:"action"`
	State     string `json:"state"`
	Detail    string `json:"detail"`
	JournalID int64  `json:"journalId"`
	PRURL     string `json:"prUrl"`
	MergeURL  string `json:"mergeUrl"`
}

func (c *client) services(ctx context.Context) ([]serviceStatus, error) {
	var out []serviceStatus
	if err := c.do(ctx, http.MethodGet, "/api/v1/services", nil, &out); err != nil {
		return nil, err
	}
	return out, nil
}

func (c *client) history(ctx context.Context, service string, n int) ([]historyEntry, error) {
	var out []historyEntry
	path := fmt.Sprintf("/api/v1/services/%s/history?n=%d", url.PathEscape(service), n)
	if err := c.do(ctx, http.MethodGet, path, nil, &out); err != nil {
		return nil, err
	}
	return out, nil
}

func (c *client) startDeploy(
	ctx context.Context, action, service, digest string,
) (int64, error) {
	var out struct {
		JournalID int64 `json:"journalId"`
	}
	path := fmt.Sprintf("/api/v1/services/%s/%s", url.PathEscape(service), action)
	body := map[string]string{"digest": digest}
	if err := c.do(ctx, http.MethodPost, path, body, &out); err != nil {
		return 0, err
	}
	return out.JournalID, nil
}

// openEvents connects to the SSE stream. The caller connects before starting a
// deploy, so no transition can slip past between the POST and the subscribe.
func (c *client) openEvents(ctx context.Context) (*bufio.Scanner, func(), error) {
	req, err := c.newRequest(ctx, http.MethodGet, "/api/v1/events", nil)
	if err != nil {
		return nil, nil, err
	}
	req.Header.Set("Accept", "text/event-stream")

	streamClient := &http.Client{}
	resp, err := streamClient.Do(req)
	if err != nil {
		return nil, nil, fmt.Errorf("connect to the event stream: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		defer func() { _ = resp.Body.Close() }()
		return nil, nil, fmt.Errorf("event stream: %s", describe(resp))
	}
	return bufio.NewScanner(resp.Body), func() { _ = resp.Body.Close() }, nil
}

// nextEvent reads frames until one belongs to journalID.
func nextEvent(scanner *bufio.Scanner, journalID int64) (event, bool) {
	for scanner.Scan() {
		line := scanner.Text()
		if !strings.HasPrefix(line, "data: ") {
			continue
		}
		var ev event
		if err := json.Unmarshal([]byte(strings.TrimPrefix(line, "data: ")), &ev); err != nil {
			continue
		}
		if journalID != 0 && ev.JournalID != journalID {
			continue
		}
		return ev, true
	}
	return event{}, false
}
