package assert

import (
	"bytes"
	"fmt"
)

// BytesEqual asserts that got and want are byte-for-byte equal.
func BytesEqual(got []byte, want []byte, msgAndArgs ...any) {
	if !bytes.Equal(got, want) {
		failWith(fmt.Sprintf("got bytes %q, want %q", got, want), msgAndArgs...)
	}
}
