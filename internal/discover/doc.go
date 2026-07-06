// Package discover lists the package closure used by one rask run.
//
// Discovery sits on top of [gotool.Tool]. It resolves the user's patterns into
// selected packages, loads the local package closure needed for fingerprinting,
// and rejects load errors before hashing starts.
//
// [Discoverer.Run] is a lifecycle wrapper over the pure discovery operation.
// It starts no internal goroutines; concurrent use is safe when each run has
// its own package channel and the supplied [gotool.Tool] implementation is safe.
package discover
