package assert

import "fmt"

// Equal asserts that got equals want.
func Equal[T comparable](got T, want T, msgAndArgs ...any) {
	if got != want {
		failWith(fmt.Sprintf("got %v, want %v", got, want), msgAndArgs...)
	}
}

// NotEqual asserts that got does not equal notWant.
func NotEqual[T comparable](got T, notWant T, msgAndArgs ...any) {
	if got == notWant {
		failWith(fmt.Sprintf("got %v, want not equal", got), msgAndArgs...)
	}
}

// NotZero asserts that got is not its zero value.
func NotZero[T comparable](got T, msgAndArgs ...any) {
	var zero T
	if got == zero {
		failWith("got zero value", msgAndArgs...)
	}
}
