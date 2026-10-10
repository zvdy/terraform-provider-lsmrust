package mockserver

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestHealthzNeedsNoCredentials(t *testing.T) {
	srv := New(Config{APIKey: "secret"})
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/healthz", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("healthz = %d, want 200", rec.Code)
	}
}

func TestAPIKeyRequiredOutsideHealthz(t *testing.T) {
	srv := New(Config{APIKey: "secret"})
	for _, auth := range []string{"", "Bearer wrong", "Bearer secre", "secret"} {
		req := httptest.NewRequest(http.MethodGet, "/v1/databases", nil)
		if auth != "" {
			req.Header.Set("Authorization", auth)
		}
		rec := httptest.NewRecorder()
		srv.ServeHTTP(rec, req)
		if rec.Code != http.StatusUnauthorized {
			t.Errorf("auth %q = %d, want 401", auth, rec.Code)
		}
	}
	req := httptest.NewRequest(http.MethodGet, "/v1/databases", nil)
	req.Header.Set("Authorization", "Bearer secret")
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("valid key = %d, want 200", rec.Code)
	}
}

func TestOversizedBodyRejected(t *testing.T) {
	srv := New(Config{})
	body := `{"name":"` + strings.Repeat("a", maxBodyBytes+1) + `"}`
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/v1/databases", strings.NewReader(body)))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("oversized body = %d, want 400", rec.Code)
	}
}
