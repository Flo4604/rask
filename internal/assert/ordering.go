package assert

import "fmt"

// Less asserts that got is less than want.
func Less[T Ordered](got T, want T, msgAndArgs ...any) {
	if got >= want {
		failWith(fmt.Sprintf("got %v, want less than %v", got, want), msgAndArgs...)
	}
}

// LessOrEqual asserts that got is less than or equal to want.
func LessOrEqual[T Ordered](got T, want T, msgAndArgs ...any) {
	if got > want {
		failWith(fmt.Sprintf("got %v, want less than or equal to %v", got, want), msgAndArgs...)
	}
}

// Greater asserts that got is greater than want.
func Greater[T Ordered](got T, want T, msgAndArgs ...any) {
	if got <= want {
		failWith(fmt.Sprintf("got %v, want greater than %v", got, want), msgAndArgs...)
	}
}

// GreaterOrEqual asserts that got is greater than or equal to want.
func GreaterOrEqual[T Ordered](got T, want T, msgAndArgs ...any) {
	if got < want {
		failWith(fmt.Sprintf("got %v, want greater than or equal to %v", got, want), msgAndArgs...)
	}
}
