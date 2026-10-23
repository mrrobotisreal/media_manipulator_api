package analyticsemit

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestHTTPDSRClientPersonScopeContract(t *testing.T) {
	var gotAuth, gotPath, gotMethod, gotBody string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth, gotPath, gotMethod = r.Header.Get("Authorization"), r.URL.Path, r.Method
		b, _ := io.ReadAll(r.Body)
		gotBody = string(b)
		_, _ = w.Write([]byte(`{"request_id":"r1","deleted":{"events":5,"sessions":2,"visitors":1,"persons":1,"consent_kept":true}}`))
	}))
	defer srv.Close()

	c := NewHTTPDSRClient(srv.URL+"/", time.Second)
	res, err := c.DeletePerson(context.Background(), "tok-abc")
	if err != nil {
		t.Fatal(err)
	}
	if gotMethod != "POST" || gotPath != "/v1/dsr/delete" || gotAuth != "Bearer tok-abc" {
		t.Errorf("request: %s %s auth=%q", gotMethod, gotPath, gotAuth)
	}
	if strings.Contains(gotBody, "visitor_id") {
		t.Errorf("person scope must not send a visitor id: %s", gotBody)
	}
	if res.RequestID != "r1" || res.Deleted.Events != 5 || res.Deleted.Persons != 1 {
		t.Errorf("result %+v", res)
	}
}

func TestHTTPDSRClientFailuresNeverLeakTheToken(t *testing.T) {
	for _, code := range []int{401, 503, 500} {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(code) }))
		_, err := NewHTTPDSRClient(srv.URL, time.Second).DeletePerson(context.Background(), "secret-token")
		srv.Close()
		if err == nil || strings.Contains(err.Error(), "secret-token") {
			t.Errorf("HTTP %d: err %v", code, err)
		}
	}
	if _, err := NewHTTPDSRClient("http://127.0.0.1:1", time.Second).DeletePerson(context.Background(), "secret-token"); err == nil || strings.Contains(err.Error(), "secret-token") {
		t.Errorf("transport error: %v", err)
	}
	if NewHTTPDSRClient("  ", 0) != nil {
		t.Error("an empty URL is 'not configured' (nil)")
	}
	if _, err := (*HTTPDSRClient)(nil).DeletePerson(context.Background(), "t"); err == nil {
		t.Error("nil client must error, not panic")
	}
	if _, err := NewHTTPDSRClient("http://x", time.Second).DeletePerson(context.Background(), " "); err == nil {
		t.Error("no token must error")
	}
}
