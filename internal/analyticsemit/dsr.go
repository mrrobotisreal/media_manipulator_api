package analyticsemit

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// MM24: the person-scope data-subject erasure request to the analytics service,
// POST {ANALYTICS_INGEST_URL}/v1/dsr/delete (media-manipulator-analytics
// internal/handlers/dsr.go, resolveScope + performDelete).
//
// Contract, read from that handler:
//   - public route, no write key; rate-limited per IP (5/min by default);
//   - "Authorization: Bearer <Firebase ID token>" selects PERSON scope: the
//     token's UID, never a body field, picks the person, and every linked
//     visitor is erased; the token must verify against the same Firebase project
//     (so it must be the caller's own, un-expired ID token);
//   - body is optional ({} is fine; visitor_id is ignored in person scope);
//   - 200 {"request_id","deleted":{"events","sessions","visitors","persons","consent_kept"}};
//     401 bad token; 503 verifier unavailable / timeout; 500 partial (retry continues).

// DSRResult is the analytics service's answer to an erasure.
type DSRResult struct {
	RequestID string `json:"request_id"`
	Deleted   struct {
		Events   int64 `json:"events"`
		Sessions int64 `json:"sessions"`
		Visitors int64 `json:"visitors"`
		Persons  int64 `json:"persons"`
	} `json:"deleted"`
}

// DSRClient erases a person's analytics data. An interface so account deletion
// is tested without the network.
type DSRClient interface {
	DeletePerson(ctx context.Context, idToken string) (DSRResult, error)
}

// HTTPDSRClient is the real client.
type HTTPDSRClient struct {
	endpoint string
	http     *http.Client
}

// NewHTTPDSRClient builds the client from the analytics base URL
// (ANALYTICS_INGEST_URL). An empty URL yields nil: the caller then reports
// analytics as "not_configured".
func NewHTTPDSRClient(baseURL string, timeout time.Duration) *HTTPDSRClient {
	base := strings.TrimRight(strings.TrimSpace(baseURL), "/")
	if base == "" {
		return nil
	}
	if timeout <= 0 {
		// The analytics handler's own erasure budget is 120 s.
		timeout = 130 * time.Second
	}
	return &HTTPDSRClient{endpoint: base + "/v1/dsr/delete", http: &http.Client{Timeout: timeout}}
}

// DeletePerson calls the endpoint. Errors carry the HTTP status but never the token.
func (c *HTTPDSRClient) DeletePerson(ctx context.Context, idToken string) (DSRResult, error) {
	var out DSRResult
	if c == nil {
		return out, errors.New("analytics DSR is not configured")
	}
	if strings.TrimSpace(idToken) == "" {
		return out, errors.New("no Firebase token to present")
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.endpoint, bytes.NewReader([]byte("{}")))
	if err != nil {
		return out, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+idToken)
	req.Header.Set("User-Agent", userAgent)
	resp, err := c.http.Do(req)
	if err != nil {
		return out, fmt.Errorf("analytics dsr request: %w", redactErr(err))
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
	if resp.StatusCode != http.StatusOK {
		return out, fmt.Errorf("analytics dsr: HTTP %d", resp.StatusCode)
	}
	if err := json.Unmarshal(body, &out); err != nil {
		return out, fmt.Errorf("analytics dsr: unreadable response: %w", err)
	}
	return out, nil
}

// redactErr drops the URL from a transport error (it never holds the token, but
// there is no reason to log the endpoint on every failure either).
func redactErr(err error) error {
	var ue interface{ Unwrap() error }
	if errors.As(err, &ue) && ue.Unwrap() != nil {
		return ue.Unwrap()
	}
	return err
}
