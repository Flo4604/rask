package assert

// True asserts that value is true.
func True(value bool, msgAndArgs ...any) {
	if !value {
		failWith("expected true", msgAndArgs...)
	}
}

// False asserts that value is false.
func False(value bool, msgAndArgs ...any) {
	if value {
		failWith("expected false", msgAndArgs...)
	}
}
