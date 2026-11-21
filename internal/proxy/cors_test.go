package proxy_test

import (
	"bytes"
	"io"
	"net/http"
	"testing"

	"quackmux/internal/wire"
)

func postWithOrigin(t *testing.T, h *harness, msg []byte, origin string) (*http.Response, []byte) {
	t.Helper()
	req, _ := http.NewRequest(http.MethodPost, h.srv.URL+"/quack", bytes.NewReader(msg))
	req.Header.Set("Content-Type", "text/plain") // a CORS "simple" request
	if origin != "" {
		req.Header.Set("Origin", origin)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	return resp, body
}

// Without authentication no browser origin is allowed by default: a web page
// (or a DNS rebinding) must not reach the gateway through a user's browser.
func TestNoneModeRefusesBrowserOrigins(t *testing.T) {
	h := newHarness(t, "", nil)
	resp, body := postWithOrigin(t, h, connectMsg("tok?user=alice"), "https://evil.example")
	if resp.StatusCode != http.StatusOK || resp.Header.Get("Access-Control-Allow-Origin") != "" {
		t.Fatalf("status %d, CORS %q", resp.StatusCode, resp.Header.Get("Access-Control-Allow-Origin"))
	}
	mustError(t, body, "not allowed")
	if n := h.sessions.Count(); n != 0 {
		t.Fatalf("%d sessions opened for a foreign origin", n)
	}
	// DuckDB's own clients send no Origin.
	_, body = postWithOrigin(t, h, connectMsg("tok?user=alice"), "")
	mustType(t, body, wire.TypeConnectionResponse)
}

// Configured origins (DuckDB-Wasm on a known site) are admitted, others not.
func TestAllowedOriginsAdmitConfiguredSites(t *testing.T) {
	h := newHarness(t, "server: {allowedOrigins: [\"https://app.example\"]}\n", nil)
	resp, body := postWithOrigin(t, h, connectMsg("tok?user=alice"), "https://app.example")
	mustType(t, body, wire.TypeConnectionResponse)
	if got := resp.Header.Get("Access-Control-Allow-Origin"); got != "https://app.example" {
		t.Fatalf("Access-Control-Allow-Origin %q", got)
	}
	_, body = postWithOrigin(t, h, connectMsg("tok?user=alice"), "https://other.example")
	mustError(t, body, "not allowed")
}
