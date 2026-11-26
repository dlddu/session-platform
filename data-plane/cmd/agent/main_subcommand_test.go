package main

import (
	"os"
	"testing"
)

// TestMain gives the test binary the same helper modes main has, so a test can
// exercise execCommandRunner for real: the runner runs the CLI under
// os.Executable, which under `go test` is this binary, and the reaper mode has
// to answer there as it does in the image.
func TestMain(m *testing.M) {
	if code, handled := runAgentSubcommand(os.Args[1:], os.Stdin, os.Stdout, os.Stderr); handled {
		os.Exit(code)
	}
	os.Exit(m.Run())
}
