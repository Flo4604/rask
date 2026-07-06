package cli

import (
	"errors"
	"testing"
)

func TestBoolFlagParsing(t *testing.T) {
	flag := Bool("verbose", "verbose output")
	if err := flag.Parse(""); err != nil {
		t.Fatalf("parse empty: %v", err)
	}
	if !flag.Value() || !flag.IsSet() {
		t.Fatalf("flag=%+v", flag)
	}
	if err := flag.Parse("maybe"); !errors.Is(err, ErrInvalidBoolValue) {
		t.Fatalf("err=%v, want invalid bool", err)
	}
}

func TestIntFlagParsing(t *testing.T) {
	flag := Int("concurrency", "suite workers")
	if flag.Value() != 0 || flag.IsSet() {
		t.Fatalf("flag=%+v, want unset zero value", flag)
	}
	if err := flag.Parse("8"); err != nil {
		t.Fatalf("parse int: %v", err)
	}
	if flag.Value() != 8 || !flag.IsSet() {
		t.Fatalf("flag=%+v, want parsed value", flag)
	}
	if err := flag.Parse("many"); !errors.Is(err, ErrInvalidIntValue) {
		t.Fatalf("err=%v, want invalid int", err)
	}
}
