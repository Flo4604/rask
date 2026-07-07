// Package discover lists the package closure used by one rask run.
//
// Discovery sits on top of [gotool.Tool]. It resolves the user's patterns into
// selected packages, loads the local package closure needed for fingerprinting,
// and rejects load errors before hashing starts.
//
// [Packages] returns a finished slice instead of streaming on a channel because
// `go list` already materializes discovery as a batch. The old stream only
// replayed a completed slice, which made cancellation and ownership harder to
// reason about without reducing memory use.
package discover
