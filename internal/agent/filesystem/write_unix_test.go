//go:build linux || darwin

package filesystem

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"sync"
	"testing"
)

func mutationTemp(t *testing.T, d *Directory) (string, FileIdentity) {
	t.Helper()
	var b [16]byte
	rand.Read(b[:])
	name := ".onprest-" + hex.EncodeToString(b[:]) + ".tmp"
	f, id, e := d.CreateTemp(name)
	if e != nil {
		t.Fatal(e)
	}
	if _, e = f.Write([]byte("new bytes")); e != nil {
		t.Fatal(e)
	}
	if e = f.Sync(); e != nil {
		t.Fatal(e)
	}
	if e = f.Close(); e != nil {
		t.Fatal(e)
	}
	id.Size = 9
	return name, id
}
func TestFilesystemWritePinnedDirectoryConcurrentNoReplaceAndOverwrite(t *testing.T) {
	base := t.TempDir()
	dir := filepath.Join(base, "business")
	os.Mkdir(dir, 0700)
	d, e := OpenDirectory(dir)
	if e != nil {
		t.Fatal(e)
	}
	defer d.Close()
	d2, e := OpenDirectory(dir)
	if e != nil {
		t.Fatal(e)
	}
	defer d2.Close()
	a, aid := mutationTemp(t, d)
	b, bid := mutationTemp(t, d2)
	successes := make(chan bool, 2)
	var wg sync.WaitGroup
	for i, entry := range []struct {
		d    *Directory
		name string
		id   FileIdentity
	}{{d, a, aid}, {d2, b, bid}} {
		_ = i
		wg.Add(1)
		go func() {
			defer wg.Done()
			yes, e := entry.d.Publish(context.Background(), entry.name, "result", false, entry.id, nil)
			if yes && e != nil {
				t.Error(e)
			}
			if !yes && !errors.Is(e, fs.ErrExist) {
				t.Error("no-replace failure", e)
			}
			successes <- yes
		}()
	}
	wg.Wait()
	close(successes)
	count := 0
	for yes := range successes {
		if yes {
			count++
		}
	}
	if count != 1 {
		t.Fatal("atomic winner count", count)
	}
	old, e := d.Inspect("result", false)
	if e != nil {
		t.Fatal(e)
	}
	temp, id := mutationTemp(t, d)
	if yes, e := d.Publish(context.Background(), temp, "result", true, id, &old); !yes || e != nil {
		t.Fatal(yes, e)
	}
	moved, e := d.Inspect(temp, false)
	if e != nil || moved.Identity != old.Identity || moved.Owner != old.Owner {
		t.Fatal("old identity not exchanged", e)
	}
	if e = d.RemoveTemp(temp, []FileIdentity{id, old}, false); e != nil {
		t.Fatal(e)
	}
	// Pinning survives an ordinary rename of the directory; new names below the
	// replacement pathname are never touched.
	renamed := dir + "-moved"
	os.Rename(dir, renamed)
	os.Mkdir(dir, 0700)
	temp, id = mutationTemp(t, d)
	if yes, e := d.Publish(context.Background(), temp, "pinned", false, id, nil); !yes || e != nil {
		t.Fatal(yes, e)
	}
	if _, e = os.Stat(filepath.Join(dir, "pinned")); !os.IsNotExist(e) {
		t.Fatal("replacement directory touched")
	}
	if _, e = os.Stat(filepath.Join(renamed, "pinned")); e != nil {
		t.Fatal(e)
	}
}
func TestFilesystemWriteRejectsTargetTempLinksAndChangedIdentity(t *testing.T) {
	for _, kind := range []string{"target_symlink", "target_hardlink", "target_changed", "target_missing", "temp_symlink", "temp_changed"} {
		t.Run(kind, func(t *testing.T) {
			base := t.TempDir()
			dir := filepath.Join(base, "business")
			os.Mkdir(dir, 0700)
			d, e := OpenDirectory(dir)
			if e != nil {
				t.Fatal(e)
			}
			defer d.Close()
			outside := filepath.Join(base, "outside")
			os.WriteFile(outside, []byte("outside preserve"), 0600)
			temp, id := mutationTemp(t, d)
			name := filepath.Join(dir, "result")
			os.WriteFile(name, []byte("previous"), 0644)
			old, e := d.Inspect("result", false)
			if e != nil {
				t.Fatal(e)
			}
			switch kind {
			case "target_symlink":
				os.Remove(name)
				os.Symlink(outside, name)
			case "target_hardlink":
				os.Remove(name)
				os.Link(outside, name)
			case "target_missing":
				os.Remove(name)
			case "target_changed":
				os.WriteFile(name, []byte("same-len"), 0644)
			case "temp_symlink":
				os.Remove(filepath.Join(dir, temp))
				os.Symlink(outside, filepath.Join(dir, temp))
			case "temp_changed":
				os.Remove(filepath.Join(dir, temp))
				os.WriteFile(filepath.Join(dir, temp), []byte("new bytes"), 0600)
			}
			if yes, e := d.Publish(context.Background(), temp, "result", true, id, &old); yes || e == nil {
				t.Fatal("changed file published", yes, e)
			}
			if b, e := os.ReadFile(outside); e != nil || string(b) != "outside preserve" {
				t.Fatal("outside changed")
			}
		})
	}
}

func TestFilesystemWriteTempPinLifetimeAndReplacedCleanup(t *testing.T) {
	pins := func(d *Directory) int { d.mu.Lock(); defer d.mu.Unlock(); return len(d.pins) }
	base := t.TempDir()
	dir := filepath.Join(base, "business")
	os.Mkdir(dir, 0700)
	d, e := OpenDirectory(dir)
	if e != nil {
		t.Fatal(e)
	}
	defer d.Close()
	temp, id := mutationTemp(t, d)
	if pins(d) != 1 {
		t.Fatal("created temp is not pinned")
	}
	if yes, e := d.Publish(context.Background(), temp, "installed", false, id, nil); !yes || e != nil || pins(d) != 0 {
		t.Fatal("install did not release the pin", yes, e, pins(d))
	}
	temp, id = mutationTemp(t, d)
	if e = d.RemoveTemp(temp, []FileIdentity{id}, false); e != nil || pins(d) != 0 {
		t.Fatal("removal did not release the pin", e, pins(d))
	}
	temp, id = mutationTemp(t, d)
	path := filepath.Join(dir, temp)
	os.Remove(path)
	os.WriteFile(path, []byte("new bytes"), 0600)
	if e = d.RemoveTemp(temp, []FileIdentity{id}, false); e == nil {
		t.Fatal("cleanup removed a replaced temp")
	}
	if b, e := os.ReadFile(path); e != nil || string(b) != "new bytes" {
		t.Fatal("replaced temp changed", e)
	}
	if pins(d) != 1 {
		t.Fatal("refused cleanup released the pin")
	}
	if e = d.Close(); e != nil || pins(d) != 0 {
		t.Fatal("close kept pins", e)
	}
}
func TestFilesystemWriteSourceDestinationDirectoryLinkRefused(t *testing.T) {
	base := t.TempDir()
	outside := t.TempDir()
	os.Mkdir(filepath.Join(base, "business"), 0700)
	r, e := OpenRoot(base)
	if e != nil {
		t.Fatal(e)
	}
	defer r.Close()
	os.Remove(filepath.Join(base, "business"))
	os.Symlink(outside, filepath.Join(base, "business"))
	if d, e := r.Directory("business", base); e == nil {
		d.Close()
		t.Fatal("linked write directory accepted")
	}
	for _, p := range []string{"../escape", ".onprest-private", "/absolute", "a\\b"} {
		if d, e := r.Directory(p, base); e == nil {
			d.Close()
			t.Fatal("unsafe path", p)
		}
	}
}

func TestFilesystemWriteExchangeGapDoesNotRollbackOtherWriter(t *testing.T) {
	dir := t.TempDir()
	d, e := OpenDirectory(dir)
	if e != nil {
		t.Fatal(e)
	}
	defer d.Close()
	os.WriteFile(filepath.Join(dir, "result"), []byte("original"), 0644)
	old, e := d.Inspect("result", false)
	if e != nil {
		t.Fatal(e)
	}
	temp, id := mutationTemp(t, d)
	exchangeBarrier = func() {
		if e := os.Rename(filepath.Join(dir, "result"), filepath.Join(dir, "original-preserved")); e != nil {
			t.Fatal(e)
		}
		if e := os.WriteFile(filepath.Join(dir, "result"), []byte("other writer"), 0644); e != nil {
			t.Fatal(e)
		}
	}
	defer func() { exchangeBarrier = nil }()
	installed, e := d.Publish(context.Background(), temp, "result", true, id, &old)
	var fail *Error
	if !installed || !errors.As(e, &fail) || fail.Code != "SOURCE_CHANGED" {
		t.Fatal("exchange ambiguity", installed, e)
	}
	// Re-exchange would restore/overwrite the independent writer. Preserve both
	// the installed business file and the unknown displaced temporary file.
	if e = d.RemoveTemp(temp, []FileIdentity{id, old}, false); e == nil {
		t.Fatal("removed unverified displaced file")
	}
	for name, want := range map[string]string{"result": "new bytes", temp: "other writer", "original-preserved": "original"} {
		b, e := os.ReadFile(filepath.Join(dir, name))
		if e != nil || string(b) != want {
			t.Fatal(name, e, string(b))
		}
	}
}

func TestFilesystemWriteExchangeGapSameInodeGrowthRemainsUnverified(t *testing.T) {
	dir := t.TempDir()
	d, e := OpenDirectory(dir)
	if e != nil {
		t.Fatal(e)
	}
	defer d.Close()
	os.WriteFile(filepath.Join(dir, "result"), []byte("original"), 0644)
	old, e := d.Inspect("result", false)
	if e != nil {
		t.Fatal(e)
	}
	temp, id := mutationTemp(t, d)
	exchangeBarrier = func() {
		if e := os.WriteFile(filepath.Join(dir, "result"), []byte("independent writer larger version"), 0644); e != nil {
			t.Fatal(e)
		}
	}
	defer func() { exchangeBarrier = nil }()
	installed, e := d.Publish(context.Background(), temp, "result", true, id, &old)
	var failure *Error
	if !installed || !errors.As(e, &failure) || failure.Code != "SOURCE_CHANGED" {
		t.Fatal(installed, e)
	}
	if e = d.RemoveTemp(temp, []FileIdentity{id, old}, false); e == nil {
		t.Fatal("changed old inode deleted")
	}
	b, e := os.ReadFile(filepath.Join(dir, temp))
	if e != nil || string(b) != "independent writer larger version" {
		t.Fatal("old writer lost", e)
	}
	b, e = os.ReadFile(filepath.Join(dir, "result"))
	if e != nil || string(b) != "new bytes" {
		t.Fatal("completed changed", e)
	}
}
