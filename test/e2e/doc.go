// Package e2e holds the end-to-end tests of quackmux. They run real DuckDB 2.0
// clients and engines with Docker Compose (compose/run.sh) and kind
// (kind/setup.sh, kind/test.sh, kind/teardown.sh); see README.md.
//
// The Go tests of this package need neither Docker nor DuckDB. They check the
// fixtures of the compose test against each other and against the gateway's
// own configuration, authentication and policy code, so that a broken fixture
// shows up in "go test ./..." and not only in a slow Docker run.
package e2e
