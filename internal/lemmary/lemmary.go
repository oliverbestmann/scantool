// Package lemmary uploads finished documents to a lemmary server.
package lemmary

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"math/rand"
	"mime/multipart"
	"net/http"
	"net/textproto"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// Client uploads documents to the lemmary API.
type Client struct {
	// BaseURL is the lemmary server, e.g. "https://lemmary.example.com".
	BaseURL string
	// APIKey authenticates the upload.
	APIKey string
	// HTTPClient sends the request. Defaults to http.DefaultClient.
	HTTPClient *http.Client
}

// Upload posts the file at path to the /api/upload endpoint as a multipart
// form, field name "file". It returns the record id from the response, so
// the caller can look the document up on the lemmary server later.
func (c *Client) Upload(ctx context.Context, path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", fmt.Errorf("lemmary: open %s: %w", path, err)
	}
	defer f.Close()

	var body bytes.Buffer
	w := multipart.NewWriter(&body)
	header := textproto.MIMEHeader{}
	header.Set("Content-Disposition", fmt.Sprintf(`form-data; name="file"; filename=%q`, filepath.Base(path)))
	header.Set("Content-Type", "application/pdf")
	part, err := w.CreatePart(header)
	if err != nil {
		return "", fmt.Errorf("lemmary: create form file: %w", err)
	}
	if _, err := io.Copy(part, f); err != nil {
		return "", fmt.Errorf("lemmary: write form file: %w", err)
	}
	if err := w.Close(); err != nil {
		return "", fmt.Errorf("lemmary: close multipart writer: %w", err)
	}

	url := strings.TrimRight(c.BaseURL, "/") + "/api/upload"
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, &body)
	if err != nil {
		return "", fmt.Errorf("lemmary: build request: %w", err)
	}
	req.Header.Set("Content-Type", w.FormDataContentType())
	req.Header.Set("Authorization", "Bearer "+c.APIKey)

	client := c.HTTPClient
	if client == nil {
		client = http.DefaultClient
	}

	resp, err := client.Do(req)
	if err != nil {
		return "", fmt.Errorf("lemmary: upload: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		msg, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return "", fmt.Errorf("lemmary: upload: status %d: %s", resp.StatusCode, strings.TrimSpace(string(msg)))
	}

	var result struct {
		ID string `json:"id"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return "", fmt.Errorf("lemmary: decode response: %w", err)
	}
	return result.ID, nil
}

// DevUploader fakes an upload without a real lemmary server: it waits Delay,
// then fails randomly with the given FailRate, e.g. 0.2 for 20% of
// requests. It's meant for exercising upload retries during development.
type DevUploader struct {
	// Delay simulates upload latency. Zero means 500ms.
	Delay time.Duration
	// FailRate is the chance (0..1) that Upload fails. Zero means 0.2.
	FailRate float64
}

func (u *DevUploader) Upload(ctx context.Context, path string) (string, error) {
	delay := u.Delay
	if delay <= 0 {
		delay = 500 * time.Millisecond
	}

	select {
	case <-time.After(delay):
	case <-ctx.Done():
		return "", ctx.Err()
	}

	failRate := u.FailRate
	if failRate <= 0 {
		failRate = 0.2
	}
	if rand.Float64() < failRate {
		return "", fmt.Errorf("lemmary: dev: simulated upload failure for %s", filepath.Base(path))
	}
	return fmt.Sprintf("dev-%d", rand.Int63()), nil
}
