package client

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func newTestClient(t *testing.T, h http.HandlerFunc) *Client {
	t.Helper()
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	c, err := New(srv.URL+"/", "secret-key", srv.Client())
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func TestNewRejectsBadEndpoint(t *testing.T) {
	for _, ep := range []string{"", "localhost:8080", "ftp://x", "://bad", "http://", "http://h?region=eu", "http://h/#frag", "http://u:p@h", "http://h?", "http://h/?"} {
		if _, err := New(ep, "", nil); err == nil {
			t.Errorf("New(%q) = nil error, want error", ep)
		}
	}
}

func TestCreateDatabaseRequestAndResponse(t *testing.T) {
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/v1/databases" {
			t.Errorf("got %s %s", r.Method, r.URL.Path)
		}
		if got := r.Header.Get("Authorization"); got != "Bearer secret-key" {
			t.Errorf("Authorization = %q", got)
		}
		if got := r.Header.Get("Content-Type"); got != "application/json" {
			t.Errorf("Content-Type = %q", got)
		}
		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Fatal(err)
		}
		want := map[string]any{
			"name": "orders", "tier": "shared",
			"resource_limits": map[string]any{"memory_mb": 512.0, "disk_gb": 10.0},
			"backup_policy":   map[string]any{"schedule": "@daily", "retention": "7d"},
		}
		if !reflect.DeepEqual(body, want) {
			t.Errorf("body = %v, want %v", body, want)
		}
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte(`{"id":"db-1","name":"orders","tier":"shared","status":"pending",
			"resource_limits":{"memory_mb":512,"disk_gb":10},"backup_policy":{"schedule":"@daily","retention":"7d"}}`))
	})

	db, err := c.CreateDatabase(context.Background(), CreateDatabaseInput{
		Name: "orders", Tier: "shared",
		ResourceLimits: ResourceLimits{MemoryMB: 512, DiskGB: 10},
		BackupPolicy:   BackupPolicy{Schedule: "@daily", Retention: "7d"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if db.ID != "db-1" || db.Status != StatusPending || db.ResourceLimits.DiskGB != 10 {
		t.Errorf("unexpected database: %+v", db)
	}
}

func TestUpdateDatabaseOmitsUnsetFields(t *testing.T) {
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPatch || r.URL.Path != "/v1/databases/db-1" {
			t.Errorf("got %s %s", r.Method, r.URL.Path)
		}
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		if _, ok := body["name"]; ok {
			t.Errorf("name should be omitted, body = %v", body)
		}
		if _, ok := body["resource_limits"]; ok {
			t.Errorf("resource_limits should be omitted, body = %v", body)
		}
		if bp, ok := body["backup_policy"].(map[string]any); !ok || bp["retention"] != "30d" {
			t.Errorf("backup_policy = %v", body["backup_policy"])
		}
		_, _ = w.Write([]byte(`{"id":"db-1","status":"ready"}`))
	})
	err := c.UpdateDatabase(context.Background(), "db-1", UpdateDatabaseInput{
		BackupPolicy: &BackupPolicy{Schedule: "@daily", Retention: "30d"},
	})
	if err != nil {
		t.Fatal(err)
	}
}

func TestErrorMapping(t *testing.T) {
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1/databases/missing":
			w.WriteHeader(http.StatusNotFound)
			_, _ = w.Write([]byte(`{"error":"database not found"}`))
		case "/v1/databases/wrong-route":
			http.NotFound(w, r)
		case "/v1/databases/conflict":
			w.WriteHeader(http.StatusConflict)
			_, _ = w.Write([]byte(`{"error":"no tier migration"}`))
		default:
			w.WriteHeader(http.StatusBadGateway)
			_, _ = w.Write([]byte("upstream down"))
		}
	})
	ctx := context.Background()

	var apiErr *APIError
	_, err := c.GetDatabase(ctx, "missing")
	if !errors.Is(err, ErrNotFound) {
		t.Errorf("API 404: got %v, want ErrNotFound", err)
	}
	if !errors.As(err, &apiErr) || apiErr.Message != "database not found" {
		t.Errorf("API 404 message lost: %v", err)
	}

	_, err = c.GetDatabase(ctx, "wrong-route")
	if errors.Is(err, ErrNotFound) {
		t.Errorf("non-API 404 must not be ErrNotFound, got %v", err)
	}
	if !errors.As(err, &apiErr) || apiErr.StatusCode != 404 {
		t.Errorf("non-API 404: got %v", err)
	}

	err = c.UpdateDatabase(ctx, "conflict", UpdateDatabaseInput{})
	if !errors.As(err, &apiErr) || apiErr.StatusCode != 409 || apiErr.Message != "no tier migration" {
		t.Errorf("409: got %v", err)
	}
	err = c.DeleteDatabase(ctx, "other")
	if !errors.As(err, &apiErr) || apiErr.StatusCode != 502 || apiErr.Message != "upstream down" {
		t.Errorf("502: got %v", err)
	}
}

func TestDeleteNoContent(t *testing.T) {
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodDelete || r.URL.Path != "/v1/access_keys/key-1" {
			t.Errorf("got %s %s", r.Method, r.URL.Path)
		}
		w.WriteHeader(http.StatusNoContent)
	})
	if err := c.DeleteAccessKey(context.Background(), "key-1"); err != nil {
		t.Fatal(err)
	}
}

func TestAccessKeyCRUD(t *testing.T) {
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		switch r.Method + " " + r.URL.Path {
		case "POST /v1/databases/db-1/access_keys":
			var body struct{ Scopes []string }
			_ = json.NewDecoder(r.Body).Decode(&body)
			if !reflect.DeepEqual(body.Scopes, []string{"read", "write"}) {
				t.Errorf("scopes = %v", body.Scopes)
			}
			w.WriteHeader(http.StatusCreated)
			_, _ = w.Write([]byte(`{"id":"key-1","database_id":"db-1","scopes":["read","write"],"secret":"s3cr3t"}`))
		case "GET /v1/access_keys/key-1":
			_, _ = w.Write([]byte(`{"id":"key-1","database_id":"db-1","scopes":["read","write"]}`))
		case "PATCH /v1/access_keys/key-1":
			w.WriteHeader(http.StatusOK)
		default:
			t.Errorf("unexpected %s %s", r.Method, r.URL.Path)
		}
	})
	ctx := context.Background()

	k, err := c.CreateAccessKey(ctx, "db-1", []string{"read", "write"})
	if err != nil || k.Secret != "s3cr3t" {
		t.Fatalf("create: %+v, %v", k, err)
	}
	k, err = c.GetAccessKey(ctx, "key-1")
	if err != nil || k.Secret != "" || k.DatabaseID != "db-1" {
		t.Fatalf("get: %+v, %v", k, err)
	}
	if err := c.UpdateAccessKey(ctx, "key-1", []string{"read"}); err != nil {
		t.Fatalf("update with empty 200 body: %v", err)
	}
}

func anyDatabase(*Database) bool { return true }

func statusServer(t *testing.T, statuses ...string) (*Client, *atomic.Int32) {
	var calls atomic.Int32
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		n := int(calls.Add(1)) - 1
		st := statuses[min(n, len(statuses)-1)]
		_ = json.NewEncoder(w).Encode(Database{ID: "db-1", Status: st})
	})
	return c, &calls
}

func TestWaitForDatabase(t *testing.T) {
	c, calls := statusServer(t, StatusPending, StatusPending, StatusReady)
	db, err := c.WaitForDatabase(context.Background(), "db-1", time.Millisecond, anyDatabase)
	if err != nil {
		t.Fatal(err)
	}
	if db.Status != StatusReady || calls.Load() != 3 {
		t.Errorf("status=%s calls=%d", db.Status, calls.Load())
	}
}

func TestWaitForDatabaseFailed(t *testing.T) {
	c, _ := statusServer(t, StatusPending, StatusFailed)
	if _, err := c.WaitForDatabase(context.Background(), "db-1", time.Millisecond, anyDatabase); err == nil {
		t.Fatal("expected error for failed status")
	}
}

func TestWaitForDatabaseTimeout(t *testing.T) {
	c, _ := statusServer(t, StatusPending)
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	_, err := c.WaitForDatabase(ctx, "db-1", 5*time.Millisecond, anyDatabase)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("got %v, want DeadlineExceeded", err)
	}
}

func TestWaitForDatabaseNotFound(t *testing.T) {
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte(`{"error":"database not found"}`))
	})
	if _, err := c.WaitForDatabase(context.Background(), "db-1", time.Millisecond, anyDatabase); !errors.Is(err, ErrNotFound) {
		t.Fatalf("got %v, want ErrNotFound", err)
	}
}

func TestWaitForDatabaseRetriesTransientErrors(t *testing.T) {
	var calls atomic.Int32
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		switch calls.Add(1) {
		case 1:
			w.WriteHeader(http.StatusServiceUnavailable)
		case 2:
			w.WriteHeader(http.StatusTooManyRequests)
		case 3:
			_ = json.NewEncoder(w).Encode(Database{ID: "db-1", Status: StatusPending})
		default:
			_ = json.NewEncoder(w).Encode(Database{ID: "db-1", Status: StatusReady})
		}
	})
	db, err := c.WaitForDatabase(context.Background(), "db-1", time.Millisecond, anyDatabase)
	if err != nil {
		t.Fatal(err)
	}
	if db.Status != StatusReady || calls.Load() != 4 {
		t.Errorf("status=%s calls=%d", db.Status, calls.Load())
	}
}

func TestWaitForDatabaseRetriesNetworkErrors(t *testing.T) {
	c, err := New("http://127.0.0.1:1", "", &http.Client{Timeout: 50 * time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	c.unreachableGrace = time.Hour
	ctx, cancel := context.WithTimeout(context.Background(), 150*time.Millisecond)
	defer cancel()
	_, err = c.WaitForDatabase(ctx, "db-1", 10*time.Millisecond, anyDatabase)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("got %v, want retries until DeadlineExceeded", err)
	}
}

func TestWaitForDatabaseStopsOnClientError(t *testing.T) {
	var calls atomic.Int32
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"error":"missing or invalid API key"}`))
	})
	_, err := c.WaitForDatabase(context.Background(), "db-1", time.Millisecond, anyDatabase)
	var apiErr *APIError
	if !errors.As(err, &apiErr) || apiErr.StatusCode != 401 || calls.Load() != 1 {
		t.Fatalf("got %v after %d calls, want immediate 401", err, calls.Load())
	}
}

func TestWaitForDatabaseCondition(t *testing.T) {
	c, calls := statusServer(t, StatusReady)
	var seen int
	db, err := c.WaitForDatabase(context.Background(), "db-1", time.Millisecond, func(*Database) bool {
		seen++
		return seen == 3
	})
	if err != nil || db.Status != StatusReady || calls.Load() != 3 {
		t.Fatalf("db=%+v err=%v calls=%d", db, err, calls.Load())
	}
}

func TestEndpointWithBasePath(t *testing.T) {
	var gotPath string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		_ = json.NewEncoder(w).Encode(Database{ID: "db-1"})
	}))
	t.Cleanup(srv.Close)
	c, err := New(srv.URL+"/api/", "", srv.Client())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := c.GetDatabase(context.Background(), "db-1"); err != nil {
		t.Fatal(err)
	}
	if gotPath != "/api/v1/databases/db-1" {
		t.Errorf("path = %q", gotPath)
	}
}

func TestWaitForDatabaseDoesNotRetryTLSErrors(t *testing.T) {
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	t.Cleanup(srv.Close)
	c, err := New(srv.URL, "", nil)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	start := time.Now()
	_, err = c.WaitForDatabase(ctx, "db-1", 10*time.Millisecond, anyDatabase)
	if err == nil || errors.Is(err, context.DeadlineExceeded) || time.Since(start) > time.Second {
		t.Fatalf("got %v after %s, want an immediate certificate error", err, time.Since(start))
	}
}

func TestWaitForDatabaseTimeoutDuringRequest(t *testing.T) {
	var calls atomic.Int32
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		if calls.Add(1) == 1 {
			_ = json.NewEncoder(w).Encode(Database{ID: "db-1", Status: StatusUpdating})
			return
		}
		<-r.Context().Done()
	})
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	_, err := c.WaitForDatabase(ctx, "db-1", time.Millisecond, anyDatabase)
	if !errors.Is(err, context.DeadlineExceeded) || !strings.Contains(err.Error(), `timed out waiting for database db-1 to become ready (last status "updating")`) {
		t.Fatalf("got %v", err)
	}
}

func TestRedirectsAreNotFollowed(t *testing.T) {
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			t.Errorf("redirect was followed: %s %s", r.Method, r.URL.Path)
		}
		http.Redirect(w, r, "/elsewhere", http.StatusMovedPermanently)
	})
	_, err := c.CreateAccessKey(context.Background(), "db-1", []string{"read"})
	var apiErr *APIError
	if !errors.As(err, &apiErr) || apiErr.StatusCode != http.StatusMovedPermanently {
		t.Fatalf("got %v, want HTTP 301 error", err)
	}
}

func TestEmptyBodyWhereOneIsRequired(t *testing.T) {
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusCreated)
	})
	_, err := c.CreateDatabase(context.Background(), CreateDatabaseInput{Name: "x"})
	if err == nil || !strings.Contains(err.Error(), "empty body") {
		t.Fatalf("got %v, want empty body error", err)
	}
}

func TestWaitForDatabaseGivesUpWhenUnreachable(t *testing.T) {
	c, err := New("http://127.0.0.1:1", "", nil)
	if err != nil {
		t.Fatal(err)
	}
	c.unreachableGrace = 100 * time.Millisecond
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	start := time.Now()
	_, err = c.WaitForDatabase(ctx, "db-1", 10*time.Millisecond, anyDatabase)
	if err == nil || !strings.Contains(err.Error(), "control plane unreachable") || time.Since(start) > 2*time.Second {
		t.Fatalf("got %v after %s", err, time.Since(start))
	}
}

func TestWaitForDatabaseCancelled(t *testing.T) {
	c, _ := statusServer(t, StatusUpdating)
	ctx, cancel := context.WithCancel(context.Background())
	time.AfterFunc(30*time.Millisecond, cancel)
	_, err := c.WaitForDatabase(ctx, "db-1", 5*time.Millisecond, anyDatabase)
	if !errors.Is(err, context.Canceled) || !strings.HasPrefix(err.Error(), "cancelled while waiting") {
		t.Fatalf("got %v", err)
	}
}
