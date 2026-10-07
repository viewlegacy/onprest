package artifacts

import (
	"context"
	"io"
	"os"
	"sync"
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

func TestArtifactsScratchKnownLengthReservationAccounting(t *testing.T) {
	for _, operation := range []string{"close", "abort", "short_write", "concurrent"} {
		t.Run(operation, func(t *testing.T) {
			s := store(t, 8)
			stage, e := s.NewScratch(context.Background(), "http.bin", "application/octet-stream", 8)
			if e != nil {
				t.Fatal(e)
			}
			if s.Usage() != 8 {
				t.Fatal("no upfront reservation", s.Usage())
			}
			if _, e = s.NewScratch(context.Background(), "second.bin", "application/octet-stream", 1); e == nil {
				t.Fatal("overbooked")
			}
			if operation == "short_write" {
				s.write = func(f *os.File, p []byte) (int, error) { return f.Write(p[:1]) }
			}
			n, e := stage.Write([]byte("body"))
			if operation == "short_write" {
				if n != 1 || e == nil {
					t.Fatal(n, e)
				}
				if s.Usage() != 5 {
					t.Fatal("short write accounting", s.Usage())
				}
			} else if n != 4 || e != nil || s.Usage() != 8 {
				t.Fatal(n, e, s.Usage())
			}
			if operation == "close" {
				if e = stage.Close(); e != nil {
					t.Fatal(e)
				}
				if s.Usage() != 4 {
					t.Fatal("unused reservation retained", s.Usage())
				}
			}
			if operation == "concurrent" {
				var wg sync.WaitGroup
				for i := 0; i < 8; i++ {
					wg.Add(1)
					go func() { defer wg.Done(); _ = stage.Abort() }()
				}
				wg.Wait()
			} else {
				if e = stage.Abort(); e != nil {
					t.Fatal(e)
				}
			}
			if s.Usage() != 0 {
				t.Fatal("reservation leaked", s.Usage())
			}
			entries, e := os.ReadDir(s.options.Directory + "/objects")
			if e != nil || len(entries) != 0 {
				t.Fatal(entries, e)
			}
		})
	}
}
