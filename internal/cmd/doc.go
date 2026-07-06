// Package cmd implements rask, a Go test runner with content-addressed result caching.
//
// The package owns command-line dispatch, cache-cleaning commands, test
// argument parsing, and the orchestration loop that connects discovery,
// fingerprinting, running, caching, and frontend services. It keeps process
// setup at the command boundary so the root executable can remain a tiny
// entrypoint.
package cmd
