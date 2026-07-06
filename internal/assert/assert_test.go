package assert_test

import (
	"errors"
	"strings"
	"testing"

	"github.com/chronark/rask/internal/assert"
)

// TestPassingAssertionsDoNotPanic protects the success path for every assertion helper.
func TestPassingAssertionsDoNotPanic(t *testing.T) {
	assert.True(true)
	assert.False(false)
	assert.Equal(7, 7)
	assert.Equal("a", "a")
	assert.NotEqual(7, 8)
	assert.NotZero("x")
	assert.Nil((*int)(nil))
	assert.NotNil(&struct{}{})
	assert.Less(1, 2)
	assert.LessOrEqual(2, 2)
	assert.Greater(3, 2)
	assert.GreaterOrEqual(3, 3)
	assert.BytesEqual([]byte("alpha"), []byte("alpha"))
	assert.NoError(nil)

	got := (assert.Failure{Message: "boom"}).Error()
	if got != "assertion failed: boom" {
		t.Fatalf("Failure.Error() = %q, want %q", got, "assertion failed: boom")
	}
}

// TestFailingAssertionsPanicWithFailure protects typed panic values with caller locations.
func TestFailingAssertionsPanicWithFailure(t *testing.T) {
	cases := []struct {
		name string
		fn   func()
		want string
	}{
		{name: "fail", fn: func() { assert.Fail("boom") }, want: "boom"},
		{name: "unreachable", fn: func() { assert.Unreachable() }, want: "unreachable"},
		{name: "true", fn: func() { assert.True(false) }, want: "expected true"},
		{name: "false", fn: func() { assert.False(true) }, want: "expected false"},
		{name: "equal", fn: func() { assert.Equal(1, 2) }, want: "want 2"},
		{name: "not equal", fn: func() { assert.NotEqual(1, 1) }, want: "want not equal"},
		{name: "not zero", fn: func() { assert.NotZero(0) }, want: "zero value"},
		{name: "nil", fn: func() { assert.Nil(1) }, want: "want nil"},
		{name: "not nil", fn: func() { assert.NotNil((*int)(nil)) }, want: "got nil"},
		{name: "less", fn: func() { assert.Less(2, 1) }, want: "less than"},
		{name: "less or equal", fn: func() { assert.LessOrEqual(2, 1) }, want: "less"},
		{name: "greater", fn: func() { assert.Greater(1, 2) }, want: "greater than"},
		{name: "greater or equal", fn: func() { assert.GreaterOrEqual(1, 2) }, want: "greater"},
		{name: "bytes", fn: func() { assert.BytesEqual([]byte("a"), []byte("b")) }, want: "bytes"},
		{name: "no error", fn: func() { assert.NoError(errors.New("bad")) }, want: "bad"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			failure := recoverAssertion(t, tc.fn)
			if failure.File == "" {
				t.Fatal("failure.File is empty, want caller file")
			}
			if failure.Line <= 0 {
				t.Fatalf("failure.Line = %d, want positive", failure.Line)
			}
			if !strings.Contains(failure.Message, tc.want) {
				t.Fatalf("message = %q, want substring %q", failure.Message, tc.want)
			}
		})
	}
}

// TestFailingAssertionsIncludeOptionalMessages protects assertion context formatting.
func TestFailingAssertionsIncludeOptionalMessages(t *testing.T) {
	cases := []struct {
		name string
		fn   func()
		want string
	}{
		{
			name: "plain message",
			fn:   func() { assert.True(false, "during manifest validation") },
			want: "expected true: during manifest validation",
		},
		{
			name: "formatted message",
			fn:   func() { assert.Equal(1, 2, "object %s", "root") },
			want: "got 1, want 2: object root",
		},
		{
			name: "additional formatted message",
			fn:   func() { assert.NoError(errors.New("bad"), "key=%d", 7) },
			want: "unexpected error: bad: key=7",
		},
		{
			name: "fail formats",
			fn:   func() { assert.Fail("bad %s", "state") },
			want: "bad state",
		},
		{
			name: "unreachable",
			fn:   func() { assert.Unreachable("state %d", 4) },
			want: "unreachable: state 4",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			failure := recoverAssertion(t, tc.fn)
			if failure.Message != tc.want {
				t.Fatalf("message = %q, want %q", failure.Message, tc.want)
			}
		})
	}
}

// TestNilAssertionsHandleTypedNilValues protects nil checks for typed nil interfaces.
func TestNilAssertionsHandleTypedNilValues(t *testing.T) {
	var pointer *int
	var bytes []byte
	var values map[string]int
	var ch chan int
	var fn func()

	assert.Nil(pointer)
	assert.Nil(bytes)
	assert.Nil(values)
	assert.Nil(ch)
	assert.Nil(fn)
}

// recoverAssertion returns the [assert.Failure] raised by fn.
func recoverAssertion(t *testing.T, fn func()) assert.Failure {
	t.Helper()

	var recovered any
	func() {
		defer func() {
			recovered = recover()
		}()
		fn()
	}()

	failure, ok := recovered.(assert.Failure)
	if !ok {
		t.Fatalf("panic = %#v, want assert.Failure", recovered)
	}
	return failure
}
