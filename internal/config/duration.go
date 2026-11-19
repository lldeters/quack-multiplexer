package config

import (
	"encoding/json"
	"fmt"
	"strconv"
	"time"
)

// Duration is a time.Duration that unmarshals from Go duration strings
// ("90s", "30m", "6h") or from a plain number of seconds. It remembers
// whether it was set explicitly, so that an explicit "0" (disabled) is not
// replaced by a default.
type Duration struct {
	time.Duration
	set bool
}

// D is a convenience constructor (an explicitly set duration).
func D(d time.Duration) Duration { return Duration{Duration: d, set: true} }

// IsSet reports whether the duration was configured explicitly.
func (d Duration) IsSet() bool { return d.set }

// MarshalJSON renders the duration as a Go duration string.
func (d Duration) MarshalJSON() ([]byte, error) {
	return json.Marshal(d.String())
}

// UnmarshalJSON accepts a duration string or a number of seconds.
func (d *Duration) UnmarshalJSON(b []byte) error {
	if string(b) == "null" {
		return nil
	}
	d.set = true
	if len(b) > 0 && b[0] == '"' {
		var s string
		if err := json.Unmarshal(b, &s); err != nil {
			return err
		}
		if s == "" || s == "0" {
			d.Duration = 0
			return nil
		}
		v, err := time.ParseDuration(s)
		if err != nil {
			// Also accept "120" as seconds.
			if secs, perr := strconv.ParseFloat(s, 64); perr == nil {
				d.Duration = time.Duration(secs * float64(time.Second))
				return nil
			}
			return fmt.Errorf("invalid duration %q: %w", s, err)
		}
		d.Duration = v
		return nil
	}
	var secs float64
	if err := json.Unmarshal(b, &secs); err != nil {
		return fmt.Errorf("invalid duration %s: %w", string(b), err)
	}
	d.Duration = time.Duration(secs * float64(time.Second))
	return nil
}
