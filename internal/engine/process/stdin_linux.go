package process

import "os"

// readOnlyPipe makes the pipe of f read-only (mode 0400). On Linux every
// process of the same user can open a pipe again through /proc/<pid>/fd/<n>,
// also for writing; an engine session with file access would get there with
// COPY ... TO '/proc/self/fd/0' and could run dot commands such as .shell on
// the CLI's connection. The mode stops that for every process without
// CAP_DAC_OVERRIDE. Descriptors that are open already, such as the
// provider's write end, keep working.
func readOnlyPipe(f *os.File) error { return f.Chmod(0o400) }
