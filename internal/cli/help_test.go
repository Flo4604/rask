package cli

import (
	"strings"
	"testing"
)

func TestShowHelpWritesConfiguredOutput(t *testing.T) {
	out := new(strings.Builder)
	cmd := &Command{
		Name:        "rask",
		Usage:       "Run tests",
		Description: "A small command.",
		AcceptsArgs: true,
		Output:      out,
		Flags:       []Flag{Bool("v", "verbose output")},
		Commands:    []*Command{{Name: "clean", Usage: "Clean cache"}},
	}
	cmd.showHelp()
	help := out.String()
	for _, want := range []string{"NAME:", "USAGE:", "COMMANDS:", "OPTIONS:", "GLOBAL OPTIONS:"} {
		if !strings.Contains(help, want) {
			t.Fatalf("help missing %q:\n%s", want, help)
		}
	}
	if strings.Contains(help, "VERSION") {
		t.Fatalf("help contains unexpected version section:\n%s", help)
	}
}

func TestWrapText(t *testing.T) {
	lines := wrapText("the quick brown fox jumps over the lazy dog", 20)
	if strings.Join(lines, "|") != "the quick brown fox|jumps over the lazy|dog" {
		t.Fatalf("lines=%v", lines)
	}
}
