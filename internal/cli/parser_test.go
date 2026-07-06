package cli

import (
	"context"
	"strings"
	"testing"
)

func TestParserAcceptsArgsFlagsAroundPositionals(t *testing.T) {
	var captured []string
	var verbose bool
	cmd := &Command{
		Name:        "run",
		AcceptsArgs: true,
		Flags:       []Flag{Bool("verbose", "verbose output")},
		Action: func(_ context.Context, c *Command) error {
			captured = c.Args()
			verbose = c.Bool("verbose")
			return nil
		},
	}
	err := cmd.parse(context.Background(), []string{"file1", "--verbose", "file2"})
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if strings.Join(captured, ",") != "file1,file2" || !verbose {
		t.Fatalf("args=%v verbose=%v", captured, verbose)
	}
}

func TestParserDoubleDashStopsFlagParsing(t *testing.T) {
	var captured []string
	cmd := &Command{
		Name:        "run",
		AcceptsArgs: true,
		Flags:       []Flag{Bool("verbose", "verbose output")},
		Action: func(_ context.Context, c *Command) error {
			captured = c.Args()
			return nil
		},
	}
	err := cmd.parse(context.Background(), []string{"--", "--verbose", "-x", "test"})
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if strings.Join(captured, ",") != "--verbose,-x,test" {
		t.Fatalf("args=%v", captured)
	}
}

func TestParserRoutesSubcommandBeforeCollectingArgs(t *testing.T) {
	ran := ""
	root := &Command{
		Name:        "rask",
		AcceptsArgs: true,
		Action:      func(context.Context, *Command) error { ran = "root"; return nil },
		Commands: []*Command{{
			Name:   "clean",
			Action: func(context.Context, *Command) error { ran = "clean"; return nil },
		}},
	}
	err := root.parse(context.Background(), []string{"clean"})
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if ran != "clean" {
		t.Fatalf("ran=%q, want clean", ran)
	}
}

func TestParserUsesDefaultActionNames(t *testing.T) {
	var captured []string
	cmd := &Command{
		Name:               "rask",
		AcceptsArgs:        true,
		DefaultActionNames: []string{"test", "t"},
		Action: func(_ context.Context, c *Command) error {
			captured = c.Args()
			return nil
		},
	}
	err := cmd.parse(context.Background(), []string{"test", "./..."})
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if strings.Join(captured, ",") != "./..." {
		t.Fatalf("args=%v", captured)
	}
}

func TestParserUnknownFlagUsesHint(t *testing.T) {
	cmd := &Command{
		Name:            "rask",
		Flags:           []Flag{Bool("v", "verbose output")},
		UnknownFlagHint: "rask supports only -v",
	}
	err := cmd.parse(context.Background(), []string{"-run"})
	if err == nil {
		t.Fatalf("parse must fail")
	}
	if !strings.Contains(err.Error(), "unknown flag: run") ||
		!strings.Contains(err.Error(), "rask supports only -v") {
		t.Fatalf("err=%q", err.Error())
	}
}
