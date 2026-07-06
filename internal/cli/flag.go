package cli

import (
	"fmt"
	"strconv"
)

var (
	// ErrInvalidBoolValue reports a boolean flag value that cannot be parsed.
	ErrInvalidBoolValue = fmt.Errorf("invalid boolean value")
)

// Flag keeps help rendering and parsing independent of concrete flag storage.
type Flag interface {
	Name() string
	Usage() string
	Parse(value string) error
	IsSet() bool
}

type baseFlag struct {
	name  string
	usage string
	set   bool
}

// Name returns the flag name without dashes.
func (b *baseFlag) Name() string { return b.name }

// Usage returns the flag's help text.
func (b *baseFlag) Usage() string { return b.usage }

// IsSet reports whether the user explicitly provided this flag.
func (b *baseFlag) IsSet() bool { return b.set }

// BoolFlag is enough for rask's current CLI, so string/default/env behavior stays out.
type BoolFlag struct {
	baseFlag
	value bool
}

// Parse sets the flag value from a string.
func (f *BoolFlag) Parse(value string) error {
	if value == "" {
		f.value = true
		f.set = true
		return nil
	}
	parsed, err := strconv.ParseBool(value)
	if err != nil {
		return fmt.Errorf("%w: %s", ErrInvalidBoolValue, value)
	}
	f.value = parsed
	f.set = true
	return nil
}

// Value returns the current boolean value.
func (f *BoolFlag) Value() bool { return f.value }

// Bool creates the only flag shape rask accepts.
func Bool(name string, usage string) *BoolFlag {
	return &BoolFlag{baseFlag: baseFlag{name: name, usage: usage}}
}
