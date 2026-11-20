package proxy

import (
	"bufio"
	"bytes"
	"log/slog"
	"strings"
	"testing"

	"quackmux/internal/config"
	"quackmux/internal/engine"
	"quackmux/internal/session"
	"quackmux/internal/wire"
)

// auditLine runs auditQuery on a PREPARE_REQUEST with sql and returns the
// logged line.
func auditLine(t *testing.T, sql string, maxQueryBytes int) string {
	t.Helper()
	var buf bytes.Buffer
	g := &Gateway{
		cfg:   &config.Config{Audit: config.AuditConfig{Enabled: true, MaxQueryBytes: maxQueryBytes}},
		audit: slog.New(slog.NewTextHandler(&buf, nil)),
	}
	h := wire.Header{Type: wire.TypePrepareRequest, ConnectionID: "c"}
	msg := wire.PrepareRequest{SQL: sql}.Encode(h)
	_, hlen, err := wire.ParseHeader(msg)
	if err != nil {
		t.Fatal(err)
	}
	g.auditQuery(bufio.NewReaderSize(bytes.NewReader(msg), 64<<10), hlen, h,
		&session.Session{ID: "s", User: "alice"}, engine.Target{ID: "e"})
	return buf.String()
}

func TestAuditLogsSQL(t *testing.T) {
	for _, sql := range []string{
		"SELECT 42",
		"SELECT * FROM 'https://example.com:8443/data/x.parquet'",
		"SELECT key, value FROM settings WHERE key = 'threads'",
	} {
		if line := auditLine(t, sql, 4096); !strings.Contains(line, sql) || strings.Contains(line, "redacted") {
			t.Errorf("%q logged as %s", sql, line)
		}
	}
}

// Credentials never reach the audit log: CREATE SECRET and the other common
// ways DuckDB statements carry them are logged without their text.
func TestAuditRedactsCredentials(t *testing.T) {
	for _, sql := range []string{
		"CREATE SECRET s3 (TYPE s3, KEY_ID 'AKIA', SECRET 'hunter2')",
		"  /* setup */ create or replace persistent secret (type gcs, key_id 'k', secret 'hunter2')",
		"SET s3_secret_access_key = 'hunter2'",
		"SET s3_session_token='hunter2'",
		"ATTACH 'host=db user=app password=hunter2' AS pg (TYPE postgres)",
		"ATTACH 'Driver={x};UID=app;PWD=hunter2' AS o",
		"ATTACH 'md:?motherduck_token=hunter2'",
		"SET azure_storage_connection_string = 'AccountName=a;AccountKey=hunter2'",
		"SELECT * FROM 'https://a.blob.core.windows.net/c/x.parquet?sv=2024&sig=hunter2'",
		"SELECT * FROM 'https://app:hunter2@example.com/x.parquet'",
	} {
		line := auditLine(t, sql, 4096)
		if strings.Contains(line, "hunter2") || !strings.Contains(line, "redacted=true") {
			t.Errorf("%q logged as %s", sql, line)
		}
	}
	// The logged text is cut inside the URL, before the "@" that marks the
	// password as one.
	sql := "SELECT * FROM 'https://app:hunter2@example.com/x.parquet'"
	cut := strings.Index(sql, "@")
	if line := auditLine(t, sql, cut); strings.Contains(line, "hunter2") {
		t.Errorf("cut URL logged as %s", line)
	}
}
