//go:build input_client_wire && (linux || darwin)

package agent

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

// The pinned real-client proof needs a browser, a JRE, the pinned generator JAR
// and the client dev dependencies, so it is selected by build tag rather than
// skipped from the default suite. Missing tooling fails instead of skipping.
func TestInputDeliveryPinnedBrowserAndGeneratedClient(t *testing.T) {
	if os.Getenv("ONPREST_IT_INPUT_CLIENT_GENERATOR_JAR") == "" {
		t.Fatal("ONPREST_IT_INPUT_CLIENT_GENERATOR_JAR is required with the input_client_wire tag")
	}
	f := productionInputHTTP(t)
	script, e := filepath.Abs("../gateway/testdata/input-clients/wire.mjs")
	if e != nil {
		t.Fatal(e)
	}
	command := exec.CommandContext(f.ctx, "node", script, f.base)
	out, e := command.CombinedOutput()
	if e != nil {
		t.Fatalf("real client wire: %v\n%s", e, out)
	}
	t.Log(string(out))
}
