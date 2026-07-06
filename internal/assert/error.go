package assert

// NoError asserts that err is nil.
func NoError(err error, msgAndArgs ...any) {
	if err != nil {
		failWith("unexpected error: "+err.Error(), msgAndArgs...)
	}
}
