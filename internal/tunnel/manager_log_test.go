package tunnel

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// D32-4: the argv written into cloudflared.log carries the tunnel token as
// `--token <value>`; the log must never hold it.
func TestRedactCloudflaredArgsMasksToken(t *testing.T) {
	cases := []struct {
		name string
		args []string
		want string
	}{
		{
			name: "token value form",
			args: []string{"tunnel", "--config", "c.yml", "--no-autoupdate", "run", "--token", "SECRET-JWT"},
			want: "tunnel --config c.yml --no-autoupdate run --token <redacted>",
		},
		{
			name: "token equals form",
			args: []string{"tunnel", "--config=c.yml", "run", "--token=SECRET-JWT"},
			want: "tunnel --config=c.yml run --token=<redacted>",
		},
		{
			name: "no secrets",
			args: []string{"tunnel", "--config", "c.yml", "run"},
			want: "tunnel --config c.yml run",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := redactCloudflaredArgs(tc.args)
			if got != tc.want {
				t.Fatalf("redacted argv = %q, want %q", got, tc.want)
			}
			if strings.Contains(got, "SECRET-JWT") {
				t.Fatalf("token leaked into %q", got)
			}
		})
	}
}

// The log is opened 0600, and a file left at 0644 by an older build is
// tightened on open.
func TestOpenCloudflaredLogFileIsPrivate(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX permission bits are not enforced on Windows")
	}
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", home)
	t.Setenv("AppData", home)
	path := cloudflaredLogPath()
	if path == "" {
		t.Fatal("no log path resolved")
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	file := openCloudflaredLogFile()
	if file == nil {
		t.Fatal("open returned no file")
	}
	defer file.Close()
	if info, err := file.Stat(); err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("mode = %v (%v), want 0600", info.Mode().Perm(), err)
	}
	// Simulate an old 0644 file.
	if err := os.Chmod(path, 0o644); err != nil {
		t.Fatal(err)
	}
	again := openCloudflaredLogFile()
	if again == nil {
		t.Fatal("reopen returned no file")
	}
	defer again.Close()
	if info, err := again.Stat(); err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("legacy file mode = %v (%v), want 0600 after reopen", info.Mode().Perm(), err)
	}
}
