package mockserver

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

const testDelay = 50 * time.Millisecond

func do(t *testing.T, h http.Handler, method, path, body string, out any) int {
	t.Helper()
	var r *http.Request
	if body == "" {
		r = httptest.NewRequest(method, path, nil)
	} else {
		r = httptest.NewRequest(method, path, bytes.NewBufferString(body))
	}
	r.Header.Set("Authorization", "Bearer k")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if out != nil && w.Body.Len() > 0 {
		if err := json.Unmarshal(w.Body.Bytes(), out); err != nil {
			t.Fatalf("decoding %s %s response %q: %v", method, path, w.Body.String(), err)
		}
	}
	return w.Code
}

const dbBody = `{"name":"orders","tier":"shared","resource_limits":{"memory_mb":512,"disk_gb":10},"backup_policy":{"schedule":"@daily","retention":"7d"}}`

func newTestServer() *Server {
	return New(Config{ProvisionDelay: testDelay, APIKey: "k"})
}

func createDB(t *testing.T, s *Server) Database {
	t.Helper()
	var db Database
	if code := do(t, s, "POST", "/v1/databases", dbBody, &db); code != http.StatusCreated {
		t.Fatalf("create: %d", code)
	}
	return db
}

func waitReady(t *testing.T, s *Server, id string) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		var db Database
		do(t, s, "GET", "/v1/databases/"+id, "", &db)
		if db.Status == StatusReady {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("database %s never became ready", id)
}

func TestAuthRequired(t *testing.T) {
	s := newTestServer()
	r := httptest.NewRequest("GET", "/v1/databases", nil)
	w := httptest.NewRecorder()
	s.ServeHTTP(w, r)
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("got %d, want 401", w.Code)
	}
}

func TestDatabaseProvisioningLifecycle(t *testing.T) {
	s := newTestServer()
	db := createDB(t, s)
	if db.Status != StatusPending {
		t.Fatalf("new database status = %q, want pending", db.Status)
	}
	waitReady(t, s, db.ID)

	var got Database
	do(t, s, "PATCH", "/v1/databases/"+db.ID, `{"backup_policy":{"schedule":"@daily","retention":"30d"}}`, &got)
	if got.Status != StatusReady || got.BackupPolicy.Retention != "30d" {
		t.Fatalf("after backup patch: %+v", got)
	}

	do(t, s, "PATCH", "/v1/databases/"+db.ID, `{"resource_limits":{"memory_mb":1024,"disk_gb":10}}`, &got)
	if got.Status != StatusUpdating || got.ResourceLimits.MemoryMB != 1024 {
		t.Fatalf("after resize patch: %+v", got)
	}
	waitReady(t, s, db.ID)
}

func TestTierChangeRejected(t *testing.T) {
	s := newTestServer()
	db := createDB(t, s)
	if code := do(t, s, "PATCH", "/v1/databases/"+db.ID, `{"tier":"dedicated"}`, nil); code != http.StatusConflict {
		t.Fatalf("got %d, want 409", code)
	}
	if code := do(t, s, "PATCH", "/v1/databases/"+db.ID, `{"tier":"shared"}`, nil); code != http.StatusOK {
		t.Fatalf("got %d, want 200", code)
	}
}

func TestValidation(t *testing.T) {
	s := newTestServer()
	cases := map[string]string{
		"bad tier":      `{"name":"x","tier":"huge","resource_limits":{"memory_mb":1,"disk_gb":1}}`,
		"no name":       `{"name":"","tier":"shared","resource_limits":{"memory_mb":1,"disk_gb":1}}`,
		"zero limits":   `{"name":"x","tier":"shared","resource_limits":{"memory_mb":0,"disk_gb":1}}`,
		"unknown field": `{"name":"x","tier":"shared","resource_limits":{"memory_mb":1,"disk_gb":1},"color":"red"}`,
		"bad json":      `{`,
	}
	for name, body := range cases {
		if code := do(t, s, "POST", "/v1/databases", body, nil); code != http.StatusBadRequest {
			t.Errorf("%s: got %d, want 400", name, code)
		}
	}
}

func TestAccessKeySecretOnlyOnCreate(t *testing.T) {
	s := newTestServer()
	db := createDB(t, s)

	var k AccessKey
	if code := do(t, s, "POST", "/v1/databases/"+db.ID+"/access_keys", `{"scopes":["read","write"]}`, &k); code != http.StatusCreated {
		t.Fatalf("create key: %d", code)
	}
	if len(k.Secret) != 64 {
		t.Fatalf("secret = %q, want 64 hex chars", k.Secret)
	}

	var got AccessKey
	do(t, s, "GET", "/v1/access_keys/"+k.ID, "", &got)
	if got.Secret != "" {
		t.Fatalf("GET returned secret %q", got.Secret)
	}
	do(t, s, "PATCH", "/v1/access_keys/"+k.ID, `{"scopes":["read"]}`, &got)
	if got.Secret != "" || len(got.Scopes) != 1 {
		t.Fatalf("PATCH returned %+v", got)
	}

	if code := do(t, s, "POST", "/v1/databases/"+db.ID+"/access_keys", `{"scopes":["root"]}`, nil); code != http.StatusBadRequest {
		t.Fatalf("invalid scope: got %d, want 400", code)
	}
	if code := do(t, s, "POST", "/v1/databases/db-nope/access_keys", `{"scopes":["read"]}`, nil); code != http.StatusNotFound {
		t.Fatalf("unknown db: got %d, want 404", code)
	}
}

func TestDeleteCascadesToKeys(t *testing.T) {
	s := newTestServer()
	db := createDB(t, s)
	var k AccessKey
	do(t, s, "POST", "/v1/databases/"+db.ID+"/access_keys", `{"scopes":["read"]}`, &k)

	if code := do(t, s, "DELETE", "/v1/databases/"+db.ID, "", nil); code != http.StatusNoContent {
		t.Fatalf("delete: %d", code)
	}
	if code := do(t, s, "GET", "/v1/databases/"+db.ID, "", nil); code != http.StatusNotFound {
		t.Fatalf("get deleted db: %d", code)
	}
	if code := do(t, s, "GET", "/v1/access_keys/"+k.ID, "", nil); code != http.StatusNotFound {
		t.Fatalf("get key of deleted db: %d", code)
	}
	if code := do(t, s, "DELETE", "/v1/databases/"+db.ID, "", nil); code != http.StatusNotFound {
		t.Fatalf("double delete: %d", code)
	}

	// The pending->ready timer firing after deletion must not resurrect it.
	time.Sleep(2 * testDelay)
	var list []Database
	do(t, s, "GET", "/v1/databases", "", &list)
	if len(list) != 0 {
		t.Fatalf("list after delete = %+v", list)
	}
}

func TestStaleTimerDoesNotFinishNewerOperation(t *testing.T) {
	const delay = 200 * time.Millisecond
	s := New(Config{ProvisionDelay: delay, APIKey: "k"})
	db := createDB(t, s)

	time.Sleep(delay * 6 / 10)
	var got Database
	do(t, s, "PATCH", "/v1/databases/"+db.ID, `{"resource_limits":{"memory_mb":2048,"disk_gb":10}}`, &got)
	if got.Status != StatusUpdating {
		t.Fatalf("after resize: %+v", got)
	}

	time.Sleep(delay * 65 / 100)
	do(t, s, "GET", "/v1/databases/"+db.ID, "", &got)
	if got.Status != StatusUpdating {
		t.Fatalf("create timer marked the resize ready early: status %q", got.Status)
	}
	waitReady(t, s, db.ID)
}
