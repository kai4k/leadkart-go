package main

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestHealth_Returns200OK(t *testing.T) {
	t.Parallel()

	srv := newServer()
	rec := httptest.NewRecorder()
	req := httptest.NewRequestWithContext(context.Background(), http.MethodGet, "/health", nil)

	srv.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusOK)
	}
	body, _ := io.ReadAll(rec.Body)
	if string(body) != "ok\n" {
		t.Fatalf("body = %q, want %q", body, "ok\n")
	}
}

func TestHealth_OnlyAcceptsGET(t *testing.T) {
	t.Parallel()

	srv := newServer()
	for _, method := range []string{http.MethodPost, http.MethodPut, http.MethodDelete} {
		method := method
		t.Run(method, func(t *testing.T) {
			t.Parallel()
			rec := httptest.NewRecorder()
			req := httptest.NewRequestWithContext(context.Background(), method, "/health", nil)
			srv.ServeHTTP(rec, req)
			if rec.Code != http.StatusMethodNotAllowed {
				t.Fatalf("status = %d, want %d", rec.Code, http.StatusMethodNotAllowed)
			}
		})
	}
}
