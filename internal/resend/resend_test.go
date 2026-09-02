package resend

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestNewClientSetsTimeout(t *testing.T) {
	c := NewClient("key", "from@example.com")
	if c.httpClient.Timeout != SendTimeout {
		t.Errorf("httpClient.Timeout = %v, want SendTimeout (%v); http.DefaultClient has none at all", c.httpClient.Timeout, SendTimeout)
	}
}

func testClient(t *testing.T, handler http.HandlerFunc) (*Client, *int) {
	t.Helper()
	hits := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits++
		handler(w, r)
	}))
	t.Cleanup(server.Close)

	return &Client{
		apiKey:     "test-key",
		from:       "kindle@example.com",
		baseURL:    server.URL,
		httpClient: server.Client(),
	}, &hits
}

func TestSendSuccess(t *testing.T) {
	client, hits := testClient(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			t.Errorf("method = %s, want POST", r.Method)
		}
		if r.URL.Path != "/emails" {
			t.Errorf("path = %s, want /emails", r.URL.Path)
		}
		if got := r.Header.Get("Authorization"); got != "Bearer test-key" {
			t.Errorf("Authorization = %q, want %q", got, "Bearer test-key")
		}

		var req sendRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Fatalf("decode request: %v", err)
		}
		if req.From != "kindle@example.com" {
			t.Errorf("from = %q, want %q", req.From, "kindle@example.com")
		}
		if len(req.To) != 1 || req.To[0] != "reader@example.com" {
			t.Errorf("to = %v, want [reader@example.com]", req.To)
		}
		if req.Text == "" {
			t.Error("text is empty; Resend requires one of text/html/react")
		}
		if len(req.Attachments) != 1 {
			t.Fatalf("attachments = %d, want 1", len(req.Attachments))
		}
		att := req.Attachments[0]
		if att.Filename != "book.epub" {
			t.Errorf("filename = %q, want %q", att.Filename, "book.epub")
		}
		content, err := base64.StdEncoding.DecodeString(att.Content)
		if err != nil {
			t.Fatalf("decode attachment content: %v", err)
		}
		if string(content) != "epub bytes" {
			t.Errorf("attachment content = %q, want %q", content, "epub bytes")
		}

		w.WriteHeader(http.StatusOK)
		json.NewEncoder(w).Encode(sendResponse{ID: "msg_123"})
	})

	id, err := client.Send(context.Background(), "reader@example.com", Attachment{
		Filename: "book.epub",
		Content:  []byte("epub bytes"),
	})
	if err != nil {
		t.Fatalf("Send: %v", err)
	}
	if id != "msg_123" {
		t.Errorf("id = %q, want %q", id, "msg_123")
	}
	if *hits != 1 {
		t.Errorf("server hits = %d, want 1", *hits)
	}
}

func TestSendAPIError(t *testing.T) {
	client, _ := testClient(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnprocessableEntity)
		json.NewEncoder(w).Encode(errorResponse{Name: "validation_error", Message: "invalid `to` field"})
	})

	_, err := client.Send(context.Background(), "reader@example.com", Attachment{
		Filename: "book.epub",
		Content:  []byte("epub bytes"),
	})
	if err == nil {
		t.Fatal("Send: want error, got nil")
	}
	if got := err.Error(); got != "resend: validation_error: invalid `to` field" {
		t.Errorf("error = %q, want it to wrap Resend's message", got)
	}
}

func TestSendAttachmentTooLarge(t *testing.T) {
	client, hits := testClient(t, func(w http.ResponseWriter, r *http.Request) {
		t.Error("request reached the server; size check should have short-circuited it")
	})

	_, err := client.Send(context.Background(), "reader@example.com", Attachment{
		Filename: "book.epub",
		Content:  make([]byte, MaxAttachmentSize+1),
	})
	if !errors.Is(err, ErrAttachmentTooLarge) {
		t.Fatalf("err = %v, want ErrAttachmentTooLarge", err)
	}
	if *hits != 0 {
		t.Errorf("server hits = %d, want 0", *hits)
	}
}
