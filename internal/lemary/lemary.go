// Package lemary uploads finished documents to a lemary server.
package lemary

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"net/textproto"
	"os"
	"path/filepath"
	"strings"
)

// Client uploads documents to the lemary API.
type Client struct {
	// BaseURL is the lemary server, e.g. "https://lemary.example.com".
	BaseURL string
	// APIKey authenticates the upload.
	APIKey string
	// HTTPClient sends the request. Defaults to http.DefaultClient.
	HTTPClient *http.Client
}

// Upload posts the file at path to the /api/upload endpoint as a multipart
// form, field name "file".
func (c *Client) Upload(ctx context.Context, path string) error {
	f, err := os.Open(path)
	if err != nil {
		return fmt.Errorf("lemary: open %s: %w", path, err)
	}
	defer f.Close()

	var body bytes.Buffer
	w := multipart.NewWriter(&body)
	header := textproto.MIMEHeader{}
	header.Set("Content-Disposition", fmt.Sprintf(`form-data; name="file"; filename=%q`, filepath.Base(path)))
	header.Set("Content-Type", "application/pdf")
	part, err := w.CreatePart(header)
	if err != nil {
		return fmt.Errorf("lemary: create form file: %w", err)
	}
	if _, err := io.Copy(part, f); err != nil {
		return fmt.Errorf("lemary: write form file: %w", err)
	}
	if err := w.Close(); err != nil {
		return fmt.Errorf("lemary: close multipart writer: %w", err)
	}

	url := strings.TrimRight(c.BaseURL, "/") + "/api/upload"
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, &body)
	if err != nil {
		return fmt.Errorf("lemary: build request: %w", err)
	}
	req.Header.Set("Content-Type", w.FormDataContentType())
	req.Header.Set("Authorization", "Bearer "+c.APIKey)

	client := c.HTTPClient
	if client == nil {
		client = http.DefaultClient
	}

	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("lemary: upload: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		msg, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return fmt.Errorf("lemary: upload: status %d: %s", resp.StatusCode, strings.TrimSpace(string(msg)))
	}
	return nil
}
