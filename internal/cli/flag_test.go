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
