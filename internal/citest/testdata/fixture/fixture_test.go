package fixture

import (
	"os"
	"testing"
)

func TestFixture(t *testing.T) {
	switch os.Getenv("CITEST_FIXTURE_MODE") {
	case "fail":
		t.Fatal("fixture failure")
	case "skip":
		t.Skip("fixture skip")
	}
}
