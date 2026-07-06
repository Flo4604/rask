package assert

import (
	"fmt"
	"reflect"
)

// Nil asserts that got is nil.
func Nil(got any, msgAndArgs ...any) {
	if !isNil(got) {
		failWith(fmt.Sprintf("got %v, want nil", got), msgAndArgs...)
	}
}

// NotNil asserts that got is not nil.
func NotNil(got any, msgAndArgs ...any) {
	if isNil(got) {
		failWith("got nil", msgAndArgs...)
	}
}

// isNil reports true for nil interfaces and typed nil values.
func isNil(value any) bool {
	if value == nil {
		return true
	}

	reflected := reflect.ValueOf(value)
	switch reflected.Kind() {
	case reflect.Chan, reflect.Func, reflect.Interface, reflect.Map, reflect.Pointer, reflect.Slice:
		return reflected.IsNil()
	default:
		return false
	}
}
