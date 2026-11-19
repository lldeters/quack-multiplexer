package process

import (
	"bytes"
	"context"
	"log/slog"
	"strings"
	"sync"
)

const (
	// outputTailBytes is how much engine output a failure message quotes.
	outputTailBytes = 2048
	// maxLogLine splits overlong output lines for the debug log.
	maxLogLine = 4096
	// redactedToken replaces the engine token in output.
	redactedToken = "[REDACTED]"
	// minTokenFragment is the shortest piece of a token that redact treats as
	// a token cut off at the edge of a text. Shorter fragments are too likely
	// to be ordinary text, and revealing them does not weaken the token.
	minTokenFragment = 4
)

// output collects an engine's combined stdout and stderr. It keeps the tail
// for failure messages and logs complete lines at debug level, both with the
// engine token redacted. It is an io.Writer for exec.Cmd.
type output struct {
	token string
	log   *slog.Logger
	debug bool

	mu      sync.Mutex
	tail    []byte // the last tailCap bytes written
	tailCap int
	line    []byte // incomplete last line, for the log
}

func newOutput(token string, log *slog.Logger) *output {
	return &output{
		token: token,
		log:   log,
		debug: log.Enabled(context.Background(), slog.LevelDebug),
		// Room for a token cut off at the start of the tail and for tokens
		// that shrink to redactedToken inside it.
		tailCap: outputTailBytes + 2*len(token),
	}
}

// Write implements io.Writer. It never fails.
func (o *output) Write(b []byte) (int, error) {
	o.mu.Lock()
	defer o.mu.Unlock()
	if len(b) >= o.tailCap {
		o.tail = append(o.tail[:0], b[len(b)-o.tailCap:]...)
	} else {
		o.tail = append(o.tail, b...)
		if over := len(o.tail) - o.tailCap; over > 0 {
			o.tail = append(o.tail[:0], o.tail[over:]...)
		}
	}
	if o.debug {
		o.logLines(b)
	}
	return len(b), nil
}

// logLines logs the complete lines in b (plus what is buffered).
func (o *output) logLines(b []byte) {
	for len(b) > 0 {
		i := bytes.IndexByte(b, '\n')
		if i < 0 {
			o.line = append(o.line, b...)
			if len(o.line) >= maxLogLine {
				o.logLine(o.line)
				o.line = o.line[:0]
			}
			return
		}
		o.line = append(o.line, b[:i]...)
		o.logLine(o.line)
		o.line = o.line[:0]
		b = b[i+1:]
	}
}

func (o *output) logLine(line []byte) {
	s := strings.TrimRight(string(line), "\r")
	if s == "" {
		return
	}
	o.log.Debug("engine output", "line", strings.ToValidUTF8(redact(s, o.token), "�"))
}

// Tail returns the last outputTailBytes of the output with the token
// redacted, trimmed of surrounding white space.
func (o *output) Tail() string {
	o.mu.Lock()
	s := string(o.tail)
	o.mu.Unlock()
	s = redact(s, o.token)
	if len(s) > outputTailBytes {
		s = s[len(s)-outputTailBytes:]
	}
	return strings.TrimSpace(strings.ToValidUTF8(s, "�"))
}

// redact replaces every occurrence of token in s, and a piece of the token
// that s starts or ends with (a token cut off by the tail buffer, or written
// incompletely), with redactedToken.
func redact(s, token string) string {
	if token == "" {
		return s
	}
	s = strings.ReplaceAll(s, token, redactedToken)
	n := len(token)
	for k := n - 1; k >= minTokenFragment; k-- {
		if strings.HasPrefix(s, token[n-k:]) {
			s = redactedToken + s[k:]
			break
		}
	}
	for k := n - 1; k >= minTokenFragment; k-- {
		if strings.HasSuffix(s, token[:k]) {
			s = s[:len(s)-k] + redactedToken
			break
		}
	}
	return s
}
