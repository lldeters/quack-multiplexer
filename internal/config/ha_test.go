package config

import (
	"strings"
	"testing"
	"time"
)

// haConfig returns a configuration with a session key (leader election needs
// one); server and ha are flow mappings without braces.
func haConfig(server, ha string) []byte {
	return []byte("auth: {mode: none}\nserver: {sessionKeyFile: /etc/quackmux/session/key" + server +
		"}\nha: {" + ha + "}\n")
}

func TestDeregistrationDelay(t *testing.T) {
	c, err := Parse(haConfig("", "enabled: true"))
	if err != nil {
		t.Fatal(err)
	}
	if got := c.HA.DeregistrationDelay.Duration; got != 3*time.Second {
		t.Errorf("default ha.deregistrationDelay = %s, want 3s", got)
	}
	c, err = Parse(haConfig("", "enabled: true, deregistrationDelay: 0s"))
	if err != nil {
		t.Fatal(err)
	}
	if got := c.HA.DeregistrationDelay.Duration; got != 0 {
		t.Errorf("explicit ha.deregistrationDelay 0s became %s", got)
	}
	// Without leader election the delay is not used; only its sign matters.
	if _, err := Parse(haConfig(", shutdownTimeout: 2s", "deregistrationDelay: 5s")); err != nil {
		t.Errorf("unused ha.deregistrationDelay rejected: %v", err)
	}
	for name, raw := range map[string][]byte{
		"negative":                   haConfig("", "enabled: true, deregistrationDelay: -1s"),
		"negative without HA":        haConfig("", "deregistrationDelay: -1s"),
		"as long as the shutdown":    haConfig(", shutdownTimeout: 3s", "enabled: true, deregistrationDelay: 3s"),
		"longer than the shutdown":   haConfig(", shutdownTimeout: 10s", "enabled: true, deregistrationDelay: 15s"),
		"no shutdown time, explicit": haConfig(", shutdownTimeout: 0s", "enabled: true, deregistrationDelay: 1s"),
	} {
		if _, err := Parse(raw); err == nil || !strings.Contains(err.Error(), "ha.deregistrationDelay") {
			t.Errorf("%s: want an error about ha.deregistrationDelay, got %v", name, err)
		}
	}
}

// Configurations without ha.deregistrationDelay stay valid with a short
// server.shutdownTimeout: the default takes at most half of it.
func TestDeregistrationDelayDefaultFitsTheShutdown(t *testing.T) {
	for shutdown, want := range map[string]time.Duration{
		"30s": 3 * time.Second,
		"6s":  3 * time.Second,
		"3s":  1500 * time.Millisecond,
		"1s":  500 * time.Millisecond,
		"0s":  0,
	} {
		c, err := Parse(haConfig(", shutdownTimeout: "+shutdown, "enabled: true"))
		if err != nil {
			t.Errorf("server.shutdownTimeout %s without ha.deregistrationDelay: %v", shutdown, err)
			continue
		}
		if got := c.HA.DeregistrationDelay.Duration; got != want {
			t.Errorf("server.shutdownTimeout %s: default ha.deregistrationDelay = %s, want %s", shutdown, got, want)
		}
	}
}
