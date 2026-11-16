package testhelpers

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"mmwatchdog/internal/primitives"
)

// KernelFiles puts files in the place of /proc and /sys for one test. Each
// name starts with proc/ or sys/, and a name that ends with a slash is an
// empty directory. It returns the directory that holds proc and sys, so a
// test can change a file.
func KernelFiles(t *testing.T, files map[string]string) string {
	t.Helper()
	root := t.TempDir()
	for name, content := range files {
		path := filepath.Join(root, name)
		if strings.HasSuffix(name, "/") {
			if err := os.MkdirAll(path, 0755); err != nil {
				t.Fatal(err)
			}
			continue
		}
		if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(content), 0644); err != nil {
			t.Fatal(err)
		}
	}
	realProc, realSys := primitives.ProcDirectory, primitives.SysDirectory
	primitives.ProcDirectory, primitives.SysDirectory = filepath.Join(root, "proc"), filepath.Join(root, "sys")
	t.Cleanup(func() { primitives.ProcDirectory, primitives.SysDirectory = realProc, realSys })
	return root
}
