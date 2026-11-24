//go:build !linux

package process

import "os"

// readOnlyPipe has nothing to do outside Linux: there /dev/fd/<n> duplicates
// a descriptor of the process itself instead of opening the pipe again, so
// the read end of the engine's standard input never becomes a write end.
func readOnlyPipe(*os.File) error { return nil }
