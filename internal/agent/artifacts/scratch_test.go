package artifacts

import (
	"context"
	"io"
	"testing"

	"github.com/viewlegacy/onprest/internal/contract"
)

func TestArtifactsScratchSharesQuotaAndCannotBePubliclyFetched(t *testing.T) {
	s := store(t, 4)
	stage, e := s.Scratch().Begin(context.Background(), "input.csv", "application/octet-stream")
	if e != nil {
		t.Fatal(e)
	}
	if _, e = stage.Write([]byte("a,b\n")); e != nil {
		t.Fatal(e)
	}
	if s.Usage() != 4 {
		t.Fatal(s.Usage())
	}
	if _, e = s.Reserve(1); e == nil {
		t.Fatal("scratch bypassed quota")
	}
	if e = stage.Close(); e != nil {
		t.Fatal(e)
	}
	r, e := stage.Finish(contract.ArtifactMetadata{Name: "input.csv", MIMEType: "application/octet-stream", SizeBytes: 4, SHA256: digest([]byte("a,b\n"))})
	if e != nil {
		t.Fatal(e)
	}
	b, e := io.ReadAll(r)
	if e != nil || string(b) != "a,b\n" {
		t.Fatal(string(b), e)
	}
	raw := stage.(*Stage)
	if _, e = s.OpenOutput(context.Background(), raw.owner, raw.id); e == nil {
		t.Fatal("scratch acquired public retention")
	}
	if e = stage.Abort(); e != nil {
		t.Fatal(e)
	}
	if s.Usage() != 4 {
		t.Fatal("reclaimed while reader open")
	}
	if e = r.Close(); e != nil {
		t.Fatal(e)
	}
	if s.Usage() != 0 {
		t.Fatal(s.Usage())
	}
}
