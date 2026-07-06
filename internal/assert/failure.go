package assert

import "fmt"

// Failure is the typed panic value used for failed assertions.
type Failure struct {
	File    string
	Line    int
	Message string
}

// Error returns a human-readable assertion failure.
func (f Failure) Error() string {
	if f.File == "" {
		return "assertion failed: " + f.Message
	}
	return fmt.Sprintf("%s:%d: assertion failed: %s", f.File, f.Line, f.Message)
}
