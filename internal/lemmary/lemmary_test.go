package lemmary_test

import (
	"context"
	"io"
	"mime"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/oliverbestmann/scantool/internal/lemmary"
)

func writeFile(t *testing.T, content string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "document.pdf")
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestUploadPostsMultipartFileWithAuth(t *testing.T) {
	var (
		gotPath  string
		gotAuth  string
		gotField string
		gotBody  string
		gotType  string
	)

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotAuth = r.Header.Get("Authorization")

		mediaType, params, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
		if err != nil || mediaType != "multipart/form-data" {
			t.Errorf("unexpected content type: %v %v", mediaType, err)
		}

		if err := r.ParseMultipartForm(1 << 20); err != nil {
			t.Fatalf("parse multipart form: %v", err)
		}
		for field, files := range r.MultipartForm.File {
			gotField = field
			gotType = files[0].Header.Get("Content-Type")
			f, err := files[0].Open()
			if err != nil {
				t.Fatal(err)
			}
			defer f.Close()
			body, err := io.ReadAll(f)
			if err != nil {
				t.Fatal(err)
			}
			gotBody = string(body)
		}
		_ = params

		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"id": "record-123", "processing_status": "pending"}`))
	}))
	defer server.Close()

	client := &lemmary.Client{BaseURL: server.URL, APIKey: "secret-key"}
	path := writeFile(t, "pdf-bytes")

	id, err := client.Upload(context.Background(), path)
	if err != nil {
		t.Fatalf("Upload: %v", err)
	}
	if id != "record-123" {
		t.Fatalf("id = %q, want record-123", id)
	}

	if gotPath != "/api/upload" {
		t.Fatalf("path = %q, want /api/upload", gotPath)
	}
	if gotAuth != "Bearer secret-key" {
		t.Fatalf("authorization = %q", gotAuth)
	}
	if gotField != "file" {
		t.Fatalf("field name = %q, want file", gotField)
	}
	if gotType != "application/pdf" {
		t.Fatalf("part content type = %q, want application/pdf", gotType)
	}
	if gotBody != "pdf-bytes" {
		t.Fatalf("body = %q", gotBody)
	}
}

func TestUploadReturnsErrorOnNonSuccessStatus(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
	}))
	defer server.Close()

	client := &lemmary.Client{BaseURL: server.URL, APIKey: "wrong-key"}
	path := writeFile(t, "pdf-bytes")

	if _, err := client.Upload(context.Background(), path); err == nil {
		t.Fatal("want error on 401, got nil")
	}
}
