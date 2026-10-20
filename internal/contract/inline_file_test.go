package contract

import (
	"errors"
	"io"
	"testing"
)

func TestInlineFileStrictSharedNormalization(t *testing.T) {
	for _, s := range []struct {
		data  string
		limit int64
		want  string
	}{{"", 0, ""}, {"AP8B", 3, ""}, {"AP8B", 2, "LIMIT_EXCEEDED"}, {"Zg==", 1, ""}, {"Zh==", 1, "invalid"}, {"Zg=", 2, "invalid"}, {"Zg==\n", 2, "invalid"}, {"Zg==Zg==", 8, "invalid"}, {"____", 8, "invalid"}} {
		t.Run(s.data, func(t *testing.T) {
			m, r, e := DecodeInlineFile(map[string]any{"name": "a.bin", "mime_type": "Application/Octet-Stream; charset=utf-8", "data": s.data}, s.limit)
			if s.want != "" {
				if e == nil {
					t.Fatal("invalid accepted")
				}
				if s.want == "LIMIT_EXCEEDED" {
					var oe *OutputError
					if !errors.As(e, &oe) || oe.Code != s.want {
						t.Fatal(e)
					}
				}
				return
			}
			if e != nil {
				t.Fatal(e)
			}
			b, e := io.ReadAll(r)
			if e != nil || m.SizeBytes != int64(len(b)) || m.MIMEType != "application/octet-stream" || len(m.SHA256) != 64 {
				t.Fatal(m, e)
			}
		})
	}
	for _, v := range []any{nil, "a", map[string]any{"name": "../a", "mime_type": "text/plain", "data": ""}, map[string]any{"name": "a", "mime_type": "text/*", "data": ""}, map[string]any{"name": "a", "mime_type": "text/plain", "data": "", "path": "secret"}} {
		if _, _, e := DecodeInlineFile(v, 8); e == nil {
			t.Fatal("invalid object accepted", v)
		}
	}
}
