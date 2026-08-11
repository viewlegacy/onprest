package agent

import (
	"bytes"
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestDecodeBoundedAgentJSONAcceptsExactLimitAndRejectsOneByteMore(t *testing.T) {
	prefix := []byte(`{"challenge":"ok"}`)
	exact := append(append([]byte(nil), prefix...), bytes.Repeat([]byte{' '}, maxAgentChallengeResponseBytes-len(prefix))...)
	var body struct {
		Challenge string `json:"challenge"`
	}
	if err := decodeBoundedAgentJSON(bytes.NewReader(exact), maxAgentChallengeResponseBytes, &body); err != nil || body.Challenge != "ok" {
		t.Fatalf("exact limit body=%#v error=%v", body, err)
	}
	if err := decodeBoundedAgentJSON(bytes.NewReader(append(exact, ' ')), maxAgentChallengeResponseBytes, &body); err == nil {
		t.Fatal("one byte over limit error=nil")
	}
}

func TestFetchAgentChallengeCancelsPartialSlowHTTPResponse(t *testing.T) {
	const rawSecret = "partial-secret-must-not-leak"
	requestCanceled := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte(`{"challenge":"` + rawSecret))
		if flusher, ok := w.(http.Flusher); ok {
			flusher.Flush()
		}
		<-r.Context().Done()
		close(requestCanceled)
	}))
	defer server.Close()
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Millisecond)
	defer cancel()
	started := time.Now()
	_, err := fetchAgentChallenge(ctx, "ws"+strings.TrimPrefix(server.URL, "http")+"/ws/agent")
	if err == nil || (!errors.Is(err, context.DeadlineExceeded) && !errors.Is(err, context.Canceled)) {
		t.Fatalf("partial slow challenge error=%v", err)
	}
	if elapsed := time.Since(started); elapsed > time.Second {
		t.Fatalf("partial slow challenge ignored context deadline for %s", elapsed)
	}
	if strings.Contains(err.Error(), rawSecret) {
		t.Fatalf("challenge error leaked partial raw body: %v", err)
	}
	select {
	case <-requestCanceled:
	case <-time.After(time.Second):
		t.Fatal("challenge HTTP request context was not canceled")
	}
}

func TestRunnerChallengeFetchUsesTenSecondDeadline(t *testing.T) {
	called := false
	_, err := fetchRunnerChallenge(t.Context(), "ws://gateway.invalid/ws/agent", func(ctx context.Context, gatewayURL string) (string, error) {
		called = true
		if gatewayURL != "ws://gateway.invalid/ws/agent" {
			t.Fatalf("gateway URL=%q", gatewayURL)
		}
		deadline, ok := ctx.Deadline()
		if !ok {
			t.Fatal("Runner challenge context has no deadline")
		}
		remaining := time.Until(deadline)
		if remaining < agentChallengeFetchTimeout-time.Second || remaining > agentChallengeFetchTimeout {
			t.Fatalf("Runner challenge deadline remaining=%s, want %s", remaining, agentChallengeFetchTimeout)
		}
		return "", errors.New("captured")
	})
	if !called || err == nil || err.Error() != "captured" {
		t.Fatalf("called=%t error=%v", called, err)
	}
}

func TestFetchAgentChallengeBoundsAndValidatesResponse(t *testing.T) {
	for _, tc := range []struct {
		name, body string
		want       string
		wantErr    bool
	}{
		{name: "valid", body: `{"challenge":"challenge-value"}`, want: "challenge-value"},
		{name: "empty", body: `{"challenge":""}`, wantErr: true},
		{name: "malformed", body: `{`, wantErr: true},
		{name: "second value", body: `{"challenge":"ok"}{}`, wantErr: true},
		{name: "non-whitespace trailing data", body: `{"challenge":"ok"}x`, wantErr: true},
		{name: "oversized challenge", body: `{"challenge":"` + strings.Repeat("x", maxAgentChallengeResponseBytes) + `"}`, wantErr: true},
		{name: "oversized unknown field", body: `{"challenge":"ok","unknown":"` + strings.Repeat("x", maxAgentChallengeResponseBytes) + `"}`, wantErr: true},
		{name: "oversized trailing whitespace", body: `{"challenge":"ok"}` + strings.Repeat(" ", maxAgentChallengeResponseBytes), wantErr: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(http.StatusCreated)
				_, _ = w.Write([]byte(tc.body))
			}))
			defer server.Close()
			challenge, err := fetchAgentChallenge(context.Background(), "ws"+strings.TrimPrefix(server.URL, "http")+"/ws/agent")
			if tc.wantErr {
				if err == nil {
					t.Fatalf("challenge=%q error=nil", challenge)
				}
				if strings.Contains(err.Error(), strings.Repeat("x", 128)) {
					t.Fatalf("error contains raw response: %v", err)
				}
				return
			}
			if err != nil || challenge != tc.want {
				t.Fatalf("challenge=%q error=%v", challenge, err)
			}
		})
	}
}

func TestFetchAgentChallengeHonorsHTTPStatusBeforeBodyDecode(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusBadGateway)
		_, _ = w.Write([]byte(strings.Repeat("x", maxAgentChallengeResponseBytes+1)))
	}))
	defer server.Close()
	_, err := fetchAgentChallenge(context.Background(), "ws"+strings.TrimPrefix(server.URL, "http")+"/ws/agent")
	if err == nil || !strings.Contains(err.Error(), "502 Bad Gateway") {
		t.Fatalf("error=%v", err)
	}
}
