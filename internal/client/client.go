// Package client talks to a store's HTTP API.
package client

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"time"

	"github.com/kasofsk/track-record/internal/api"
)

// ErrNotFound is a 404: no live record, or no log.
var ErrNotFound = errors.New("not found")

// StatusError is a response with a status the client did not expect.
type StatusError struct {
	Code    int
	Message string
}

func (e *StatusError) Error() string {
	return fmt.Sprintf("%d %s: %s", e.Code, http.StatusText(e.Code), e.Message)
}

// Client makes requests to one store with one token, each capped at a
// timeout.
type Client struct {
	base      *url.URL
	token     string
	timeout   time.Duration
	userAgent string
	http      *http.Client
}

// New returns a client for the store at server.
func New(server, token string, timeout time.Duration, userAgent string) (*Client, error) {
	u, err := url.Parse(server)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return nil, fmt.Errorf("the server %q is not an http or https URL", server)
	}
	return &Client{base: u, token: token, timeout: timeout, userAgent: userAgent, http: &http.Client{}}, nil
}

// Record returns the live record for key.
func (c *Client) Record(ctx context.Context, namespace, key string) (api.Record, error) {
	status, data, err := c.request(ctx, http.MethodGet, nil, 0, "v1", "records", namespace, key)
	if err != nil {
		return api.Record{}, err
	}
	if err := expect(status, data, http.StatusOK); err != nil {
		return api.Record{}, err
	}
	var r api.Record
	if err := json.Unmarshal(data, &r); err != nil {
		return api.Record{}, fmt.Errorf("malformed record: %w", err)
	}
	return r, nil
}

// PutRecord records a pass. The bool reports whether this request created
// the record rather than finding one already there.
func (c *Client) PutRecord(ctx context.Context, namespace, key string, body api.PutRecord) (bool, error) {
	b, err := json.Marshal(body)
	if err != nil {
		return false, err
	}
	status, data, err := c.request(ctx, http.MethodPut, bytes.NewReader(b), int64(len(b)), "v1", "records", namespace, key)
	if err != nil {
		return false, err
	}
	return status == http.StatusCreated, expect(status, data, http.StatusCreated, http.StatusOK)
}

// Log returns a stored log's bytes.
func (c *Client) Log(ctx context.Context, namespace, digest string) ([]byte, error) {
	status, data, err := c.request(ctx, http.MethodGet, nil, 0, "v1", "logs", namespace, digest)
	if err != nil {
		return nil, err
	}
	return data, expect(status, data, http.StatusOK)
}

// PutLog stores size bytes read from r under digest.
func (c *Client) PutLog(ctx context.Context, namespace, digest string, r io.Reader, size int64) error {
	status, data, err := c.request(ctx, http.MethodPut, r, size, "v1", "logs", namespace, digest)
	if err != nil {
		return err
	}
	return expect(status, data, http.StatusCreated, http.StatusOK)
}

func (c *Client) request(ctx context.Context, method string, body io.Reader, size int64, path ...string) (int, []byte, error) {
	ctx, cancel := context.WithTimeout(ctx, c.timeout)
	defer cancel()
	if size == 0 {
		body = http.NoBody
	}
	req, err := http.NewRequestWithContext(ctx, method, c.base.JoinPath(path...).String(), body)
	if err != nil {
		return 0, nil, err
	}
	req.ContentLength = size
	req.Header.Set("Authorization", "Bearer "+c.token)
	req.Header.Set("User-Agent", c.userAgent)
	resp, err := c.http.Do(req)
	if err != nil {
		return 0, nil, err
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(resp.Body)
	if err != nil {
		return 0, nil, err
	}
	return resp.StatusCode, data, nil
}

func expect(status int, data []byte, ok ...int) error {
	for _, s := range ok {
		if status == s {
			return nil
		}
	}
	if status == http.StatusNotFound {
		return ErrNotFound
	}
	var e api.Error
	if json.Unmarshal(data, &e) != nil || e.Error == "" {
		e.Error = string(bytes.TrimSpace(data))
	}
	return &StatusError{Code: status, Message: e.Error}
}
