package app

import (
	"bytes"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestUpdateVariable(t *testing.T) {
	for _, shell := range []bool{false, true} {
		before := []byte("OTHER=keep\r\n" + cacheVariable + "=old\r\n" + cacheVariable + "=duplicate\r\n")
		value := "/cache/a 'quoted' directory"
		after := updateVariable(before, value, shell)
		if !bytes.Contains(after, []byte("OTHER=keep\r\n")) || bytes.Count(after, []byte(cacheVariable+"=")) != 1 {
			t.Fatalf("unrelated line or duplicate preservation failed: %q", after)
		}
		if !bytes.Equal(after, updateVariable(after, value, shell)) {
			t.Fatal("not idempotent")
		}
		if shell && !bytes.Contains(after, []byte("'\"'\"'")) {
			t.Fatal("shell quotes not escaped")
		}
	}
}

func TestRunnerConfiguration(t *testing.T) {
	root := t.TempDir()
	runner, cache := filepath.Join(root, "runner"), filepath.Join(root, "cache")
	listener := "Runner.Listener"
	if runtime.GOOS == "windows" {
		listener += ".exe"
	}
	writeTestFile(t, filepath.Join(runner, "bin", listener), nil)
	p := filepath.Join(runner, ".env")
	writeTestFile(t, p, []byte("OTHER=preserved\n"))
	for _, dry := range []bool{true, false, false} {
		if err := configureEnvironment("runner", runner, cache, dry, &bytes.Buffer{}); err != nil {
			t.Fatal(err)
		}
		b, err := os.ReadFile(p)
		if err != nil || !strings.Contains(string(b), "OTHER=preserved\n") {
			t.Fatal("unrelated environment lost")
		}
		if dry && bytes.Contains(b, []byte(cacheVariable)) {
			t.Fatal("dry run changed runner")
		}
		if !dry && bytes.Count(b, []byte(cacheVariable+"=")) != 1 {
			t.Fatal("configuration not idempotent")
		}
	}
}

func TestConfigurationScopeAndPlatformFiles(t *testing.T) {
	for _, tt := range []struct{ scope, runner string }{{"", ""}, {"bad", ""}, {"runner", ""}, {"user", "runner"}} {
		if err := configureEnvironment(tt.scope, tt.runner, "", true, &bytes.Buffer{}); err == nil {
			t.Errorf("accepted scope=%s runner=%s", tt.scope, tt.runner)
		}
	}
	for _, goos := range []string{"linux", "darwin", "windows"} {
		for _, scope := range []string{"runner", "user", "system"} {
			files, shell, err := configurationFiles(goos, scope, "home", "runner")
			if err != nil {
				t.Fatal(err)
			}
			if goos == "linux" && scope == "system" && (len(files) != 1 || files[0] != "/etc/environment" || shell) {
				t.Fatal("Linux upstream behavior changed")
			}
			if goos == "windows" && scope != "runner" && len(files) != 0 {
				t.Fatal("Windows must use registry")
			}
		}
	}
}

func TestCommandRejectsInvalidCombinations(t *testing.T) {
	for _, args := range [][]string{
		{"download", "o/r@v1", "--latest-major-versions", "0"},
		{"download", "o/r", "--latest-versions-per-major", "-1"},
		{"download", "o/r", "--format", "rar"},
		{"cleanup", "o/r"},
		{"configure", "--scope", "runner"},
	} {
		cmd := NewCommand(&bytes.Buffer{}, &bytes.Buffer{})
		cmd.SetArgs(args)
		if err := cmd.Execute(); err == nil {
			t.Fatalf("invalid args accepted: %v", args)
		}
	}
}
