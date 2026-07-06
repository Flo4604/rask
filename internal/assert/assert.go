package assert

import (
	"fmt"
	"path/filepath"
	"runtime"
)

// Fail panics with an assertion failure.
func Fail(message string, args ...any) {
	failDepth(formatMessage(message, args...), 2)
}

// Unreachable panics because an impossible code path was reached.
func Unreachable(msgAndArgs ...any) {
	failWith("unreachable", msgAndArgs...)
}

// failWith appends optional context to message and reports its caller's caller.
func failWith(message string, msgAndArgs ...any) {
	failDepth(messageWithContext(message, msgAndArgs...), 3)
}

// failDepth panics with a [Failure] annotated from callerDepth.
func failDepth(message string, callerDepth int) {
	_, file, line, ok := runtime.Caller(callerDepth)
	if !ok {
		panic(Failure{Message: message})
	}
	panic(Failure{
		File:    filepath.Base(file),
		Line:    line,
		Message: message,
	})
}

// messageWithContext appends optional assertion context to a generated message.
func messageWithContext(message string, msgAndArgs ...any) string {
	if len(msgAndArgs) == 0 {
		return message
	}
	return message + ": " + formatMessage(msgAndArgs[0], msgAndArgs[1:]...)
}

// formatMessage formats assertion messages with fmt-style optional arguments.
func formatMessage(message any, args ...any) string {
	format, ok := message.(string)
	if ok && len(args) > 0 {
		return fmt.Sprintf(format, args...)
	}
	if len(args) == 0 {
		return fmt.Sprint(message)
	}
	values := make([]any, 0, len(args)+1)
	values = append(values, message)
	values = append(values, args...)
	return fmt.Sprint(values...)
}
