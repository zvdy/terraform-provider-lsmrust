package client

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/hashicorp/terraform-plugin-log/tflog"
)

const (
	StatusPending  = "pending"
	StatusUpdating = "updating"
	StatusReady    = "ready"
	StatusFailed   = "failed"
)

var ErrNotFound = errors.New("not found")

type APIError struct {
	StatusCode int
	Message    string
	// fromAPI is set when the body is the control plane's own JSON error, as
	// opposed to e.g. a proxy's or router's generic 404 page.
	fromAPI bool
}

func (e *APIError) Error() string {
	return fmt.Sprintf("control plane returned HTTP %d: %s", e.StatusCode, e.Message)
}

func (e *APIError) Is(target error) bool {
	return target == ErrNotFound && e.StatusCode == http.StatusNotFound && e.fromAPI
}

type transportError struct {
	err error
}

func (e *transportError) Error() string { return e.err.Error() }
func (e *transportError) Unwrap() error { return e.err }

func isRetryable(err error) bool {
	var apiErr *APIError
	if errors.As(err, &apiErr) {
		return apiErr.StatusCode >= 500 || apiErr.StatusCode == http.StatusTooManyRequests
	}
	var tErr *transportError
	if !errors.As(err, &tErr) {
		return false
	}
	var dnsErr *net.DNSError
	var certErr *tls.CertificateVerificationError
	var hostErr x509.HostnameError
	var authErr x509.UnknownAuthorityError
	var recordErr tls.RecordHeaderError
	switch {
	case errors.As(err, &dnsErr) && dnsErr.IsNotFound,
		errors.As(err, &certErr),
		errors.As(err, &hostErr),
		errors.As(err, &authErr),
		errors.As(err, &recordErr):
		return false
	}
	return true
}

type ResourceLimits struct {
	MemoryMB int64 `json:"memory_mb"`
	DiskGB   int64 `json:"disk_gb"`
}

type BackupPolicy struct {
	Schedule  string `json:"schedule"`
	Retention string `json:"retention"`
}

type Database struct {
	ID             string         `json:"id,omitempty"`
	Name           string         `json:"name"`
	Tier           string         `json:"tier"`
	Status         string         `json:"status,omitempty"`
	ResourceLimits ResourceLimits `json:"resource_limits"`
	BackupPolicy   BackupPolicy   `json:"backup_policy"`
}

type CreateDatabaseInput struct {
	Name           string         `json:"name"`
	Tier           string         `json:"tier"`
	ResourceLimits ResourceLimits `json:"resource_limits"`
	BackupPolicy   BackupPolicy   `json:"backup_policy"`
}

type UpdateDatabaseInput struct {
	Name           *string         `json:"name,omitempty"`
	ResourceLimits *ResourceLimits `json:"resource_limits,omitempty"`
	BackupPolicy   *BackupPolicy   `json:"backup_policy,omitempty"`
}

type AccessKey struct {
	ID         string   `json:"id"`
	DatabaseID string   `json:"database_id"`
	Scopes     []string `json:"scopes"`
	Secret     string   `json:"secret,omitempty"`
}

type Client struct {
	endpoint *url.URL
	apiKey   string
	http     *http.Client

	unreachableGrace time.Duration
}

func New(endpoint, apiKey string, httpClient *http.Client) (*Client, error) {
	u, err := url.Parse(strings.TrimRight(endpoint, "/"))
	if err != nil {
		return nil, fmt.Errorf("invalid endpoint %q: %w", endpoint, err)
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return nil, fmt.Errorf("invalid endpoint %q: scheme must be http or https", endpoint)
	}
	if u.Host == "" {
		return nil, fmt.Errorf("invalid endpoint %q: missing host", endpoint)
	}
	if u.RawQuery != "" || u.ForceQuery || u.Fragment != "" || u.User != nil {
		return nil, fmt.Errorf("invalid endpoint %q: must not contain a query, fragment or credentials", endpoint)
	}
	hc := http.Client{Timeout: 30 * time.Second}
	if httpClient != nil {
		hc = *httpClient
	}
	hc.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	return &Client{endpoint: u, apiKey: apiKey, http: &hc, unreachableGrace: time.Minute}, nil
}

func (c *Client) CreateDatabase(ctx context.Context, in CreateDatabaseInput) (*Database, error) {
	var out Database
	if err := c.do(ctx, http.MethodPost, "/v1/databases", in, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

func (c *Client) GetDatabase(ctx context.Context, id string) (*Database, error) {
	var out Database
	if err := c.do(ctx, http.MethodGet, "/v1/databases/"+url.PathEscape(id), nil, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

func (c *Client) UpdateDatabase(ctx context.Context, id string, in UpdateDatabaseInput) error {
	return c.do(ctx, http.MethodPatch, "/v1/databases/"+url.PathEscape(id), in, nil)
}

func (c *Client) DeleteDatabase(ctx context.Context, id string) error {
	return c.do(ctx, http.MethodDelete, "/v1/databases/"+url.PathEscape(id), nil, nil)
}

func (c *Client) WaitForDatabase(ctx context.Context, id string, interval time.Duration, done func(*Database) bool) (*Database, error) {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	var last *Database
	var lastErr error
	var unreachableSince time.Time
	for {
		db, err := c.GetDatabase(ctx, id)
		var tErr *transportError
		if errors.As(err, &tErr) && ctx.Err() == nil {
			if unreachableSince.IsZero() {
				unreachableSince = time.Now()
			} else if time.Since(unreachableSince) >= c.unreachableGrace {
				return last, fmt.Errorf("control plane unreachable for %s while waiting for database %s: %w", c.unreachableGrace, id, err)
			}
		} else {
			unreachableSince = time.Time{}
		}
		switch {
		case err == nil:
			last, lastErr = db, nil
			if db.Status == StatusFailed {
				return db, fmt.Errorf("database %s entered status %q", id, db.Status)
			}
			if db.Status == StatusReady && done(db) {
				return db, nil
			}
		case ctx.Err() != nil:
		case isRetryable(err):
			lastErr = err
			tflog.Warn(ctx, "retrying after transient control plane error", map[string]any{"database_id": id, "error": err.Error()})
		default:
			return nil, err
		}
		select {
		case <-ctx.Done():
			return last, waitTimeoutError(ctx, id, last, lastErr)
		case <-ticker.C:
		}
	}
}

func waitTimeoutError(ctx context.Context, id string, last *Database, lastErr error) error {
	detail := "no status received"
	switch {
	case lastErr != nil:
		detail = "last error: " + lastErr.Error()
	case last != nil:
		detail = fmt.Sprintf("last status %q", last.Status)
	}
	if errors.Is(ctx.Err(), context.Canceled) {
		return fmt.Errorf("cancelled while waiting for database %s to become ready (%s): %w", id, detail, ctx.Err())
	}
	return fmt.Errorf("timed out waiting for database %s to become ready (%s): %w", id, detail, ctx.Err())
}

func (c *Client) CreateAccessKey(ctx context.Context, databaseID string, scopes []string) (*AccessKey, error) {
	var out AccessKey
	body := map[string][]string{"scopes": scopes}
	if err := c.do(ctx, http.MethodPost, "/v1/databases/"+url.PathEscape(databaseID)+"/access_keys", body, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

func (c *Client) GetAccessKey(ctx context.Context, id string) (*AccessKey, error) {
	var out AccessKey
	if err := c.do(ctx, http.MethodGet, "/v1/access_keys/"+url.PathEscape(id), nil, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

func (c *Client) UpdateAccessKey(ctx context.Context, id string, scopes []string) error {
	body := map[string][]string{"scopes": scopes}
	return c.do(ctx, http.MethodPatch, "/v1/access_keys/"+url.PathEscape(id), body, nil)
}

func (c *Client) DeleteAccessKey(ctx context.Context, id string) error {
	return c.do(ctx, http.MethodDelete, "/v1/access_keys/"+url.PathEscape(id), nil, nil)
}

func (c *Client) do(ctx context.Context, method, path string, in, out any) error {
	var body io.Reader
	if in != nil {
		b, err := json.Marshal(in)
		if err != nil {
			return fmt.Errorf("encoding request: %w", err)
		}
		body = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.endpoint.String()+path, body)
	if err != nil {
		return err
	}
	req.Header.Set("Accept", "application/json")
	if in != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if c.apiKey != "" {
		req.Header.Set("Authorization", "Bearer "+c.apiKey)
	}

	resp, err := c.http.Do(req)
	if err != nil {
		return &transportError{fmt.Errorf("%s %s: %w", method, path, err)}
	}
	defer resp.Body.Close()

	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		msg, fromAPI := errorMessage(resp.Body)
		return &APIError{StatusCode: resp.StatusCode, Message: msg, fromAPI: fromAPI}
	}
	if out == nil {
		return nil
	}
	b, err := io.ReadAll(resp.Body)
	if err != nil {
		return &transportError{fmt.Errorf("reading %s %s response: %w", method, path, err)}
	}
	if len(bytes.TrimSpace(b)) == 0 {
		return fmt.Errorf("%s %s: control plane returned HTTP %d with an empty body", method, path, resp.StatusCode)
	}
	if err := json.Unmarshal(b, out); err != nil {
		return fmt.Errorf("decoding %s %s response: %w", method, path, err)
	}
	return nil
}

func errorMessage(r io.Reader) (string, bool) {
	b, _ := io.ReadAll(io.LimitReader(r, 64<<10))
	var e struct {
		Error string `json:"error"`
	}
	if json.Unmarshal(b, &e) == nil && e.Error != "" {
		return e.Error, true
	}
	msg := strings.TrimSpace(string(b))
	if msg == "" {
		msg = "empty response body"
	}
	return msg, false
}
