package main

import "testing"

// TestDisplayBaseURL verifies the deep-link host selection: public base
// wins when set (with trailing slash trimmed), apiURL is the fallback.
// This is the helper that fixes the UB-2 "search_notes returns localhost
// URLs" bug — the formatter calls it to decide which host to surface.
func TestDisplayBaseURL(t *testing.T) {
	cases := []struct {
		name      string
		baseURL   string
		publicURL string
		want      string
	}{
		{"public set wins", "http://localhost:8443", "https://ub.example.com", "https://ub.example.com"},
		{"public empty falls back to base", "http://localhost:8443", "", "http://localhost:8443"},
		{"public with trailing slash gets trimmed", "http://localhost:8443", "https://ub.example.com/", "https://ub.example.com"},
		{"public with multiple trailing slashes", "http://localhost:8443", "https://ub.example.com///", "https://ub.example.com"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c := newAPIClient(tc.baseURL, tc.publicURL, "", "", "")
			if got := c.displayBaseURL(); got != tc.want {
				t.Errorf("displayBaseURL: got %q, want %q", got, tc.want)
			}
		})
	}
}
