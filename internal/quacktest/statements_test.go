package quacktest

import (
	"bytes"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"quackmux/internal/wire"
)

func post(t *testing.T, s *Server, msg []byte) []byte {
	t.Helper()
	resp, err := http.Post(s.URL()+"/quack", wire.ContentType, bytes.NewReader(msg))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func connect(t *testing.T, s *Server) string {
	t.Helper()
	b := post(t, s, wire.ConnectionRequest{AuthString: s.Token, MinSupportedVersion: 3, MaxSupportedVersion: 3,
		HeartbeatTimeoutSeconds: 60}.Encode(wire.Header{}))
	h, _, err := wire.DecodeConnectionResponse(b)
	if err != nil {
		t.Fatal(err)
	}
	return h.ConnectionID
}

// errorText returns the message of an ERROR_RESPONSE ("" for other answers).
func errorText(t *testing.T, b []byte) string {
	t.Helper()
	h, _, err := wire.ParseHeader(b)
	if err != nil {
		t.Fatal(err)
	}
	if h.Type != wire.TypeErrorResponse {
		return ""
	}
	e, err := wire.DecodeErrorResponse(b)
	if err != nil {
		t.Fatal(err)
	}
	return e.Message
}

// The fake engine answers like the Quack server of DuckDB v2.0.0 did in the
// probes the query time limit is based on: a STREAM is fetched batch by
// batch up to the terminal answer, a CANCEL for another query is refused,
// one for the running query interrupts the requests that wait for it.
func TestStatements(t *testing.T) {
	s := NewServer("tok")
	defer s.Close()
	c := connect(t, s)
	prepare := func(sql string, u wire.Hugeint) []byte {
		return post(t, s, wire.PrepareRequest{SQL: sql, QueryUUID: u}.Encode(wire.Header{ConnectionID: c}))
	}
	fetch := func(u wire.Hugeint, i uint64) []byte {
		return post(t, s, wire.FetchRequest{QueryUUID: u, BatchIndex: i}.Encode(wire.Header{ConnectionID: c}))
	}
	q1 := wire.Hugeint{Upper: 1, Lower: 1}
	if b := prepare("STREAM 2 10", q1); !bytes.Contains(b, []byte{0x03, 0x00, 0x01}) {
		t.Fatalf("STREAM answer %x does not say needs_more_fetch", b)
	}
	for i, want := range []bool{false, false, true} {
		f, err := wire.DecodeFetchResponseHead(fetch(q1, uint64(i+1)))
		if err != nil || f.Terminal() != want {
			t.Fatalf("FETCH %d: %+v, %v, want terminal %v", i+1, f, err, want)
		}
	}
	if msg := errorText(t, fetch(wire.Hugeint{Upper: 9}, 1)); msg != MsgResultClosed {
		t.Fatalf("FETCH of another query: %q", msg)
	}
	if msg := errorText(t, fetch(q1, 0)); msg != MsgNoBatchIndex {
		t.Fatalf("FETCH of batch 0: %q", msg)
	}

	q2 := wire.Hugeint{Upper: 2, Lower: 2}
	prepare("STREAM 3 60000", q2)
	waiting := make(chan []byte, 1)
	go func() { waiting <- fetch(q2, 1) }()
	if msg := errorText(t, post(t, s, wire.EncodeCancel(c, q1))); !strings.HasPrefix(msg, "Attempted to cancel a different query") {
		t.Fatalf("CANCEL of the previous query: %q", msg)
	}
	time.Sleep(50 * time.Millisecond)
	if msg := errorText(t, post(t, s, wire.EncodeCancel(c, q2))); msg != "" {
		t.Fatalf("CANCEL of the running query: %q", msg)
	}
	select {
	case b := <-waiting:
		if msg := errorText(t, b); msg != MsgCancelled {
			t.Fatalf("waiting FETCH answered %q", msg)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the waiting FETCH was not interrupted")
	}
	if msg := errorText(t, fetch(q2, 2)); msg != MsgCancelled {
		t.Fatalf("FETCH after the cancel: %q", msg)
	}
	// An idle wildcard CANCEL succeeds, the next query runs.
	if msg := errorText(t, post(t, s, wire.EncodeCancel(c, wire.Hugeint{}))); msg != "" {
		t.Fatalf("wildcard CANCEL: %q", msg)
	}
	if msg := errorText(t, prepare("SELECT 1", wire.Hugeint{Upper: 3})); msg != "" {
		t.Fatalf("next query: %q", msg)
	}
	var cancels []wire.Hugeint
	for _, r := range s.Received() {
		if r.Type == wire.TypeCancelRequest {
			cancels = append(cancels, r.QueryUUID)
		}
	}
	if len(cancels) != 3 || cancels[0] != q1 || cancels[1] != q2 || !cancels[2].IsZero() {
		t.Fatalf("recorded cancels %v", cancels)
	}
}

// With Supersede, a PREPARE interrupts the statement before it.
func TestSupersede(t *testing.T) {
	s := NewServer("tok")
	defer s.Close()
	s.Supersede = true
	c := connect(t, s)
	first := make(chan []byte, 1)
	go func() {
		first <- post(t, s, wire.PrepareRequest{SQL: "SLEEP 60000", QueryUUID: wire.Hugeint{Lower: 1}}.Encode(wire.Header{ConnectionID: c}))
	}()
	time.Sleep(50 * time.Millisecond)
	post(t, s, wire.PrepareRequest{SQL: "SELECT 1", QueryUUID: wire.Hugeint{Lower: 2}}.Encode(wire.Header{ConnectionID: c}))
	select {
	case b := <-first:
		if msg := errorText(t, b); msg != MsgSuperseded {
			t.Fatalf("superseded query answered %q", msg)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the first query was not superseded")
	}
}

// Like the Quack server, DISCONNECT removes the connection at once but lets
// the requests of it that are in flight finish: their statement runs on and
// is interrupted only when the last of them returns. Without a request in
// flight it is interrupted at once.
func TestDisconnectWithARequestInFlight(t *testing.T) {
	s := NewServer("tok")
	defer s.Close()
	c := connect(t, s)
	s.mu.Lock()
	cn := s.conns[c]
	s.mu.Unlock()
	q1 := wire.Hugeint{Upper: 1, Lower: 1}
	start := time.Now()
	answer := make(chan []byte, 1)
	go func() {
		answer <- post(t, s, wire.PrepareRequest{SQL: "SLEEP 300", QueryUUID: q1}.Encode(wire.Header{ConnectionID: c}))
	}()
	deadline := time.Now().Add(10 * time.Second)
	for {
		s.mu.Lock()
		running := cn.inFlight == 1 && cn.stmt != nil
		s.mu.Unlock()
		if running {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the PREPARE did not start")
		}
		time.Sleep(5 * time.Millisecond)
	}
	if msg := errorText(t, post(t, s, wire.EncodeEmpty(wire.Header{Type: wire.TypeDisconnect, ConnectionID: c}))); msg != "" {
		t.Fatalf("DISCONNECT: %q", msg)
	}
	if msg := errorText(t, post(t, s, wire.EncodeCancel(c, q1))); msg != "Invalid connection id" {
		t.Fatalf("CANCEL after the DISCONNECT: %q", msg)
	}
	var b []byte
	select {
	case b = <-answer:
	case <-time.After(10 * time.Second):
		t.Fatal("the PREPARE did not finish")
	}
	if msg := errorText(t, b); msg != "" {
		t.Fatalf("the PREPARE in flight across the DISCONNECT answered %q", msg)
	}
	if d := time.Since(start); d < 300*time.Millisecond {
		t.Fatalf("the PREPARE answered after %s, before its statement was over", d)
	}
	s.mu.Lock()
	aborted, reason := cn.stmt.aborted, cn.stmt.reason
	s.mu.Unlock()
	if !aborted || reason != msgDisconnected {
		t.Fatalf("after the last request the statement is aborted=%v (%q)", aborted, reason)
	}

	c2 := connect(t, s)
	s.mu.Lock()
	cn2 := s.conns[c2]
	s.mu.Unlock()
	post(t, s, wire.PrepareRequest{SQL: "STREAM 3 60000", QueryUUID: q1}.Encode(wire.Header{ConnectionID: c2}))
	post(t, s, wire.EncodeEmpty(wire.Header{Type: wire.TypeDisconnect, ConnectionID: c2}))
	s.mu.Lock()
	aborted = cn2.stmt.aborted
	s.mu.Unlock()
	if !aborted {
		t.Fatal("a DISCONNECT without a request in flight left the statement running")
	}
}
