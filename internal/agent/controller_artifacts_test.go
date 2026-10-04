package agent

import (
	"bytes"
	"context"
	"io"
	"path/filepath"
	"testing"
	"time"

	"github.com/viewlegacy/onprest/internal/agent/artifacts"
	"github.com/viewlegacy/onprest/internal/agent/providers"
	"github.com/viewlegacy/onprest/internal/contract"
)

func TestControllerProductionArtifactSpoolLifetimeAndLimit(t *testing.T) {
	for _, quota := range []int64{3, 8} {
		t.Run(string(rune('0'+quota)), func(t *testing.T) {
			s, e := artifacts.Open(artifacts.Options{Directory: filepath.Join(t.TempDir(), "spool"), QuotaBytes: quota, MaxPending: 2, MaxObjects: 8, TransferTimeout: time.Minute})
			if e != nil {
				t.Fatal(e)
			}
			defer s.Close()
			p := &controllerFixture{fn: func(ctx context.Context, in map[string]any, effects *providers.Effects) (contract.ProviderResult, error) {
				effects.Begin().Committed()
				return contract.ProviderResult{Files: []contract.ProviderFile{{Name: "file.bin", MIMEType: "application/octet-stream", Reader: bytes.NewReader([]byte("result"))}}}, nil
			}}
			_, prepared := controller(t, controllerConfig(t, "files"), p, nil, nil)
			snapshot, _ := prepared.Snapshot("run")
			owner := artifacts.Owner{RequestID: "request", Capability: "run", CallerBinding: "caller", DefinitionHash: snapshot.DefinitionHash(), AuthGeneration: "connection"}
			scope, e := s.Output(owner, artifacts.OutputLimits{MaxFileBytes: 8, MaxTotalBytes: 8, MaxFiles: 1, TTL: time.Minute})
			if e != nil {
				t.Fatal(e)
			}
			ctl, e := NewExecutionController(prepared, scope, nil)
			if e != nil {
				t.Fatal(e)
			}
			req := request()
			req.DefinitionHash = owner.DefinitionHash
			out, failure := ctl.Execute(context.Background(), req)
			if quota < 6 {
				if out != nil || failure == nil || failure.Code != "LIMIT_EXCEEDED" || failure.Outcome != "committed" || s.Usage() != 0 {
					t.Fatalf("quota failure %v %v %d", out, failure, s.Usage())
				}
				return
			}
			if failure != nil {
				t.Fatal(failure)
			}
			d, e := scope.Retain(out.Files)
			if e != nil {
				t.Fatal(e)
			}
			if e = out.Close(); e != nil {
				t.Fatal(e)
			}
			reader, e := s.OpenOutput(context.Background(), owner, d[0].ID)
			if e != nil {
				t.Fatal(e)
			}
			b, e := io.ReadAll(reader)
			if e != nil || string(b) != "result" {
				t.Fatalf("post-controller data %q %v", b, e)
			}
			_ = reader.Close()
			if s.Usage() != 6 {
				t.Fatal("result retention lost")
			}
		})
	}
}
