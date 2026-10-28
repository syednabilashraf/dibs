package cli

import (
	"os"
	"testing"
)

func TestMain(m *testing.M) {
	if os.Getenv("DIBS_TEST_AS_CLI") == "1" {
		os.Exit(Run(os.Args[1:], os.Stdin, os.Stdout, os.Stderr))
	}
	os.Exit(m.Run())
}
