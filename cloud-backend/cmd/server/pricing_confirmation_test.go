package main

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
)

func TestVerifyAcceptedPrice(t *testing.T) {
	cases := []struct {
		name, input string
		actual      int64
		wantOK      bool
		status      int
	}{
		{"legacy omitted", "", 800, true, 200},
		{"matching", "800", 800, true, 200},
		{"stale quote", "500", 800, false, http.StatusConflict},
		{"invalid amount", "abc", 800, false, http.StatusBadRequest},
		{"zero amount", "0", 800, false, http.StatusBadRequest},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			values := url.Values{"accepted_total_price_paise": {tc.input}}
			req := httptest.NewRequest(http.MethodPost, "/api/k/kiosk/upload", strings.NewReader(values.Encode()))
			req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
			if err := req.ParseForm(); err != nil {
				t.Fatal(err)
			}
			w := httptest.NewRecorder()
			ok := verifyAcceptedPrice(w, req, tc.actual)
			if ok != tc.wantOK {
				t.Fatalf("accepted=%v want=%v", ok, tc.wantOK)
			}
			if !ok && w.Code != tc.status {
				t.Fatalf("status=%d want=%d", w.Code, tc.status)
			}
		})
	}
}
