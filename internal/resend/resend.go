// Package resend sends an email with one attachment through Resend's API —
// DESIGN.md's chosen transport for send-to-Kindle. It is a thin wrapper
// over the single POST /emails endpoint, not a general mail abstraction:
// there is no Sender interface, because nothing else implements one yet.
package resend

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
)

const defaultBaseURL = "https://api.resend.com"

// MaxAttachmentSize is the largest attachment Send will attempt. Resend
// caps a whole message (headers, JSON, and the base64-encoded attachment)
// at 40MB; base64 inflates raw bytes by ~4/3, so that allows roughly 30MB
// of raw attachment. 28MB leaves headroom for headers and the rest of the
// JSON body. Amazon's own 200MB Send-to-Kindle cap is well above this and
// isn't the binding constraint (per DESIGN.md).
const MaxAttachmentSize = 28 * 1024 * 1024

// ErrAttachmentTooLarge is returned by Send, wrapped with the actual size,
// when an attachment exceeds MaxAttachmentSize. Checked before any request
// is made, so callers can distinguish "too large, tell the user clearly"
// from a Resend or network failure.
var ErrAttachmentTooLarge = errors.New("attachment exceeds resend size limit")

// Attachment is a file to send.
type Attachment struct {
	Filename string
	Content  []byte
}

// Client sends mail through Resend's API as the configured from address.
type Client struct {
	apiKey     string
	from       string
	baseURL    string
	httpClient *http.Client
}

// NewClient builds a Client that authenticates with apiKey and sends as
// from.
func NewClient(apiKey, from string) *Client {
	return &Client{
		apiKey:     apiKey,
		from:       from,
		baseURL:    defaultBaseURL,
		httpClient: http.DefaultClient,
	}
}

type sendRequest struct {
	From        string              `json:"from"`
	To          []string            `json:"to"`
	Subject     string              `json:"subject"`
	Text        string              `json:"text"`
	Attachments []attachmentPayload `json:"attachments"`
}

type attachmentPayload struct {
	Filename string `json:"filename"`
	Content  string `json:"content"`
}

type sendResponse struct {
	ID string `json:"id"`
}

type errorResponse struct {
	Name    string `json:"name"`
	Message string `json:"message"`
}

// Send emails a to a single recipient. Amazon's Send-to-Kindle only looks
// at the attachment, so the subject and body are static rather than
// caller-configurable. On success it returns Resend's message id, for a
// future send_log to correlate against delivery/bounce webhooks.
func (c *Client) Send(ctx context.Context, to string, a Attachment) (id string, err error) {
	if len(a.Content) > MaxAttachmentSize {
		return "", fmt.Errorf("%w: %s is %d bytes, max %d", ErrAttachmentTooLarge, a.Filename, len(a.Content), MaxAttachmentSize)
	}

	body, err := json.Marshal(sendRequest{
		From:    c.from,
		To:      []string{to},
		Subject: "Sent from Bookshelf",
		Attachments: []attachmentPayload{{
			Filename: a.Filename,
			Content:  base64.StdEncoding.EncodeToString(a.Content),
		}},
	})
	if err != nil {
		return "", fmt.Errorf("marshal request: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+"/emails", bytes.NewReader(body))
	if err != nil {
		return "", fmt.Errorf("build request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+c.apiKey)
	req.Header.Set("Content-Type", "application/json")

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return "", fmt.Errorf("send request: %w", err)
	}
	defer resp.Body.Close()

	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", fmt.Errorf("read response: %w", err)
	}

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		var apiErr errorResponse
		if err := json.Unmarshal(respBody, &apiErr); err == nil && apiErr.Message != "" {
			return "", fmt.Errorf("resend: %s: %s", apiErr.Name, apiErr.Message)
		}
		return "", fmt.Errorf("resend: unexpected status %d: %s", resp.StatusCode, respBody)
	}

	var sendResp sendResponse
	if err := json.Unmarshal(respBody, &sendResp); err != nil {
		return "", fmt.Errorf("parse response: %w", err)
	}
	return sendResp.ID, nil
}
