package upstream

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"slices"
	"testing"
	"time"

	"quackmux/internal/config"
	"quackmux/internal/quacktest"
	"quackmux/internal/wire"
)

func testClient() *Client {
	return New(config.UpstreamConfig{
		DialTimeout: config.D(time.Second), ControlTimeout: config.D(5 * time.Second),
		MaxIdleConnsPerEngine: 4, IdleConnTimeout: config.D(time.Minute),
	})
}

// The listing names every other connection of the engine, and the control
// connection is gone afterwards.
func TestConnectionsListsOthers(t *testing.T) {
	srv := quacktest.NewServer("engine-token")
	defer srv.Close()
	a, b := srv.Connect(), srv.Connect()
	ids, err := testClient().Connections(context.Background(), srv.URL(), "engine-token")
	if err != nil {
		t.Fatal(err)
	}
	slices.Sort(ids)
	want := []string{a, b}
	slices.Sort(want)
	if !slices.Equal(ids, want) {
		t.Fatalf("listed %v, want %v", ids, want)
	}
	if got := srv.Connections(); len(got) != 2 {
		t.Fatalf("the engine holds %d connections after the listing, want 2 (the control connection stayed)", len(got))
	}
	var sql []string
	for _, r := range srv.Received() {
		if r.Type == wire.TypePrepareRequest {
			sql = append(sql, r.SQL)
		}
	}
	if len(sql) != 1 || sql[0] != wire.ActiveConnectionsSQL {
		t.Fatalf("queries %q, want only %q", sql, wire.ActiveConnectionsSQL)
	}
}

func TestConnectionsErrors(t *testing.T) {
	srv := quacktest.NewServer("engine-token")
	defer srv.Close()
	c := testClient()
	ctx := context.Background()

	var ee *EngineError
	if _, err := c.Connections(ctx, srv.URL(), "wrong"); !errors.As(err, &ee) || ee.Response.Message != "Authentication failed" {
		t.Fatalf("wrong token: %v", err)
	}
	srv.SetListingError("Authorization callback failed: not allowed")
	if _, err := c.Connections(ctx, srv.URL(), "engine-token"); !errors.As(err, &ee) || ee.Response.Message != "Authorization callback failed: not allowed" {
		t.Fatalf("rejected listing: %v", err)
	}
	if n := len(srv.Connections()); n != 0 {
		t.Fatalf("a failed listing left %d connections behind", n)
	}
}

// Answers that are not a complete listing are errors, never a list.
func TestConnectionsRejectsOtherAnswers(t *testing.T) {
	for name, answer := range map[string][]byte{
		"more to fetch":  wire.VarcharResult{Names: []string{"connection_id"}, Columns: []wire.VarcharColumn{{}}, NeedsMoreFetch: true}.Encode(),
		"other column":   wire.VarcharResult{Names: []string{"query"}, Columns: []wire.VarcharColumn{{Values: []string{"x"}}}}.Encode(),
		"two columns":    wire.VarcharResult{Names: []string{"connection_id", "query"}, Columns: []wire.VarcharColumn{{Values: []string{"a"}}, {Values: []string{"b"}}}}.Encode(),
		"null id":        wire.VarcharResult{Names: []string{"connection_id"}, Columns: []wire.VarcharColumn{{Values: []string{""}, Null: []bool{true}}}}.Encode(),
		"no result rows": wire.EncodeEmpty(wire.Header{Type: wire.TypeSuccessResponse}),
	} {
		disconnected := false
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			body, _ := io.ReadAll(r.Body)
			h, _, _ := wire.ParseHeader(body)
			switch h.Type {
			case wire.TypeConnectionRequest:
				_, _ = w.Write(wire.ConnectionResponse{QuackVersion: wire.ProtocolVersion, HeartbeatTimeoutSeconds: 60}.Encode(wire.Header{ConnectionID: "CTRL"}))
			case wire.TypePrepareRequest:
				_, _ = w.Write(answer)
			case wire.TypeDisconnect:
				disconnected = true
				_, _ = w.Write(wire.EncodeEmpty(wire.Header{Type: wire.TypeSuccessResponse}))
			}
		}))
		if ids, err := testClient().Connections(context.Background(), srv.URL, "t"); err == nil {
			t.Errorf("%s: listed %v", name, ids)
		}
		srv.Close()
		if !disconnected {
			t.Errorf("%s: the control connection was not closed", name)
		}
	}
}

// An engine that answers the control handshake with a CONNECTION_RESPONSE
// the gateway cannot decode (here an unknown property 5, as a newer server
// might send) created the control connection all the same: the listing
// fails and closes it.
func TestConnectionsClosesUndecodableControlConnection(t *testing.T) {
	disconnected := ""
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		h, _, _ := wire.ParseHeader(body)
		switch h.Type {
		case wire.TypeConnectionRequest:
			ok := wire.ConnectionResponse{QuackVersion: wire.ProtocolVersion, HeartbeatTimeoutSeconds: 60}.Encode(wire.Header{ConnectionID: "CTRL"})
			// Property 5 (uvarint 1) before the end of the object.
			_, _ = w.Write(append(append([]byte{}, ok[:len(ok)-2]...), 0x05, 0x00, 0x01, 0xff, 0xff))
		case wire.TypeDisconnect:
			disconnected = h.ConnectionID
			_, _ = w.Write(wire.EncodeEmpty(wire.Header{Type: wire.TypeSuccessResponse}))
		default:
			_, _ = w.Write(wire.ErrorResponse{Message: "unexpected"}.Encode())
		}
	}))
	defer srv.Close()
	if ids, err := testClient().Connections(context.Background(), srv.URL, "t"); err == nil {
		t.Fatalf("listed %v", ids)
	}
	if disconnected != "CTRL" {
		t.Fatalf("disconnected %q, want the control connection CTRL", disconnected)
	}
}
