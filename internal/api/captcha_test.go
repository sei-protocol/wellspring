package api

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestSiteverify(t *testing.T) {
	for _, tc := range []struct {
		name   string
		reply  string
		ok     bool
		denied bool
	}{
		{"success", `{"success":true}`, true, false},
		{"bad token", `{"success":false,"error-codes":["invalid-input-response"]}`, false, true},
		{"bad secret", `{"success":false,"error-codes":["invalid-input-secret"]}`, false, false},
		{"not json", `<html>502</html>`, false, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.FormValue("secret") != "s3cret" || r.FormValue("response") != "tok" || r.FormValue("remoteip") != "192.0.2.1" {
					t.Errorf("form = %v", r.Form)
				}
				_, _ = w.Write([]byte(tc.reply))
			}))
			defer srv.Close()
			v := siteverify{url: srv.URL, secret: "s3cret", client: srv.Client()}
			err := v.Verify(context.Background(), "tok", "192.0.2.1")
			if (err == nil) != tc.ok || errors.Is(err, errCaptcha) != tc.denied {
				t.Fatalf("err = %v", err)
			}
		})
	}
}

func TestSiteverifyFailsClosed(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		t.Error("empty token reached the provider")
	}))
	v := siteverify{url: srv.URL, secret: "s3cret", client: srv.Client()}
	if err := v.Verify(context.Background(), "", ""); !errors.Is(err, errCaptcha) {
		t.Fatalf("empty token: err = %v", err)
	}
	srv.Close()
	if err := v.Verify(context.Background(), "tok", ""); err == nil {
		t.Fatal("unreachable provider: want error")
	}
}
