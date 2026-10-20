package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"testing"
	"time"

	"github.com/viewlegacy/onprest/internal/agent/providers"
	"github.com/viewlegacy/onprest/internal/contract"
	"github.com/viewlegacy/onprest/internal/protocol"
)

// Repeated because a select over a completed write and a canceled ctx picks either one.
func TestTransportSignedCommitAfterInputControlWriteIsNotAborted(t *testing.T) {
	for _, kind := range []string{"input_accept", "input_ready"} {
		t.Run(kind, func(t *testing.T) {
			for i := 0; i < 16; i++ {
				signedCommitAfterInputControlWrite(t, kind, fmt.Sprintf("ordered-%d", i))
			}
		})
	}
}

func signedCommitAfterInputControlWrite(t *testing.T, kind, id string) {
	t.Helper()
	held := make(chan struct{})
	decided := make(chan error, 1)
	previous := inputControlWaitHook
	inputControlWaitHook = func(ctx context.Context, frame string) func(error) {
		if frame != kind {
			return func(error) {}
		}
		close(held)
		select {
		case <-ctx.Done():
		case <-time.After(3 * time.Second):
			t.Error("commit did not end the input phase")
		}
		return func(e error) { decided <- e }
	}
	defer func() { inputControlWaitHook = previous }()
	r, p, conn, cleanup := signedInputPeer(t)
	defer cleanup()
	original := p.fn
	p.fn = func(ctx context.Context, input map[string]any, effects *providers.Effects) (contract.ProviderResult, error) {
		select {
		case e := <-decided:
			decided <- e
		case <-time.After(3 * time.Second):
			return contract.ProviderResult{}, fmt.Errorf("submitter did not decide")
		}
		return original(ctx, input, effects)
	}
	inputBegin(t, r, conn, id, time.Now().Add(2*time.Second))
	ready := inputFile(t, conn, id, "transfer-"+id, "/doc", true)
	select {
	case <-held:
	case <-time.After(3 * time.Second):
		t.Fatal("input control hook was not reached before commit")
	}
	conn.WriteText(protocol.MustJSON(protocol.Frame{Type: "request_commit", ID: id, Params: map[string]any{"doc": nil}, Files: []protocol.FileBinding{{Pointer: "/doc", FileID: ready.FileID}}}))
	raw, e := conn.ReadText()
	var response protocol.Response
	if e != nil || json.Unmarshal(raw, &response) != nil || response.Error != nil || p.calls.Load() != 1 {
		t.Fatalf("written %s aborted the executing request: %s %v", kind, raw, e)
	}
	select {
	case e := <-decided:
		if e != nil {
			t.Fatalf("written %s reported as failed: %v", kind, e)
		}
	default:
		t.Fatal("submitter result was not observed")
	}
}
