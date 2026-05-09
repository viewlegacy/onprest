//go:build integration

package it

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestAgentDoesNotDeclareInboundHTTPListener(t *testing.T) {
	repo := repoRoot(t)
	for _, rel := range []string{
		filepath.Join("cmd", "agent"),
		filepath.Join("internal", "agent"),
	} {
		root := filepath.Join(repo, rel)
		err := filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if d.IsDir() || !strings.HasSuffix(path, ".go") {
				return nil
			}
			b, err := os.ReadFile(path)
			if err != nil {
				return err
			}
			source := string(b)
			for _, forbidden := range []string{"ListenAndServe", "http.Listen", "net.Listen("} {
				if strings.Contains(source, forbidden) {
					t.Fatalf("%s contains inbound listener marker %q", path, forbidden)
				}
			}
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
	}
}
