// Package assert provides always-on assertions for impossible internal states.
//
// rask follows TigerBeetle's assertion discipline: assert densely in
// production code so programmer bugs fail fast instead of silently corrupting
// state. Assertions are for facts the program must have already made
// true: initialized internal state, bounded counts and sizes, sorted keys,
// valid state transitions, checksum relationships after verification, and
// unreachable control-flow paths.
//
// Assertions are not error handling. Operating errors from the outside world,
// such as missing objects, checksum failures in external bytes, stale
// conditional writes, malformed input, and context cancellation, must be
// returned as ordinary errors. Use assertions only for internal invariants
// whose violation means the code is wrong.
//
// Prefer several precise assertions over one compound assertion. For example:
//
//	assert.NotZero(entryCount)
//	assert.LessOrEqual(entryCount, blockEntryCountMax)
//
// This gives a clearer failure and documents both the positive space and the
// bound. Pair important assertions at independent boundaries, such as before
// writing a checksummed block and after reading it back.
//
// Assertion helpers accept optional message arguments after their required
// values. When the first optional argument is a format string, the remaining
// arguments are formatted with fmt.Sprintf and appended to the assertion's
// generated failure message.
package assert
