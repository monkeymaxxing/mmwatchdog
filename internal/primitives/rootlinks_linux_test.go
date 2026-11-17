//go:build linux

package primitives_test

import (
	"errors"
	"io"
	"os"
	"path/filepath"
	"testing"

	"mmwatchdog/internal/primitives"
)

// A file that MMWatchdog reads as root for alerts is reached only through
// links that root owns: a link in a directory above the file that another
// account owns is refused and named, and a link that root owns, such as
// /var/run, is followed.
func TestAFileIsReadOnlyThroughLinksThatRootOwns(t *testing.T) {
	secrets := t.TempDir()
	if err := os.WriteFile(filepath.Join(secrets, "app.log"), []byte("the secret\n"), 0600); err != nil {
		t.Fatal(err)
	}
	app := t.TempDir()
	link := filepath.Join(app, "log")
	if err := os.Symlink(secrets, link); err != nil {
		t.Fatal(err)
	}
	asRoot := os.Geteuid() == 0
	if asRoot {
		if err := os.Lchown(link, 65534, 65534); err != nil {
			t.Fatal(err)
		}
	}
	var refused primitives.LinkNotOwnedByRoot
	if f, _, err := primitives.OpenRegularFileNotALink(filepath.Join(link, "app.log")); !errors.As(err, &refused) || refused.Link != link {
		if f != nil {
			f.Close()
		}
		t.Fatalf("the link %s in a directory above the file was followed or not named: %v", link, err)
	}
	// A real path opens.
	f, about, err := primitives.OpenRegularFileNotALink(filepath.Join(secrets, "app.log"))
	if err != nil || !about.IsRegularFile {
		t.Fatal(err)
	}
	f.Close()
	if !asRoot {
		return
	}
	// A link that root owns is followed, at the end of the path and above it.
	if err := os.Lchown(link, 0, 0); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{filepath.Join(link, "app.log")} {
		f, _, err := primitives.OpenRegularFileNotALink(path)
		if err != nil {
			t.Fatal(path, err)
		}
		content, _ := io.ReadAll(f)
		f.Close()
		if string(content) != "the secret\n" {
			t.Fatalf("%s: %q", path, content)
		}
	}
	last := filepath.Join(app, "last.log")
	if err := os.Symlink(filepath.Join(secrets, "app.log"), last); err != nil {
		t.Fatal(err)
	}
	if f, _, err := primitives.OpenRegularFileNotALink(last); err != nil {
		t.Fatal(err)
	} else {
		f.Close()
	}
}
