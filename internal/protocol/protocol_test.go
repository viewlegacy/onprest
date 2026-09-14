package protocol

import "testing"

func TestAgentVerifyAuthMessageUsesDedicatedDomainAndFields(t *testing.T) {
	got := string(AgentVerifyAuthMessage("/ws/agent/verify", "2026-09-12T00:00:00Z", "nonce", "challenge"))
	want := "onprest-agent-verify-v1\n/ws/agent/verify\n2026-09-12T00:00:00Z\nnonce\nchallenge"
	if got != want {
		t.Fatalf("message=%q, want %q", got, want)
	}
	if string(AgentAuthMessage("/ws/agent", "2026-09-12T00:00:00Z", "nonce", "challenge", "handshake")) == got {
		t.Fatal("verify and WebSocket authentication domains were reused")
	}
}
