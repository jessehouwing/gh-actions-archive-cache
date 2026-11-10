package app

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
)

const cacheVariable = "ACTIONS_RUNNER_ACTION_ARCHIVE_CACHE"

var fixtureConfigRoot string

func updateVariable(data []byte, value string, shell bool) []byte {
	newline := "\n"
	if strings.Contains(string(data), "\r\n") {
		newline = "\r\n"
	}
	line := cacheVariable + "=" + value
	if shell {
		line = "export " + cacheVariable + "='" + strings.ReplaceAll(value, "'", "'\"'\"'") + "'"
	}
	lines := strings.Split(strings.ReplaceAll(string(data), "\r\n", "\n"), "\n")
	var output []string
	inserted := false
	for _, old := range lines {
		trimmed := strings.TrimSpace(old)
		trimmed = strings.TrimPrefix(trimmed, "export ")
		if strings.HasPrefix(trimmed, cacheVariable+"=") {
			if !inserted {
				output = append(output, line)
				inserted = true
			}
			continue
		}
		output = append(output, old)
	}
	if !inserted {
		if len(output) > 0 && output[len(output)-1] == "" {
			output = output[:len(output)-1]
		}
		output = append(output, line, "")
	}
	return []byte(strings.Join(output, newline))
}

func configurationFiles(goos, scope, home, runner string) ([]string, bool, error) {
	if scope == "runner" {
		return []string{filepath.Join(runner, ".env")}, false, nil
	}
	if goos == "windows" {
		return nil, false, nil
	}
	if goos != "linux" && goos != "darwin" {
		return nil, false, fmt.Errorf("unsupported platform %s", goos)
	}
	if scope == "system" {
		if goos == "linux" {
			return []string{"/etc/environment"}, false, nil
		}
		return []string{"/etc/profile", "/etc/bashrc", "/etc/zprofile", "/etc/zshrc"}, true, nil
	}
	if goos == "darwin" {
		return []string{filepath.Join(home, ".bash_profile"), filepath.Join(home, ".bashrc"),
			filepath.Join(home, ".zprofile"), filepath.Join(home, ".zshrc")}, true, nil
	}
	return []string{filepath.Join(home, ".profile"), filepath.Join(home, ".bashrc"),
		filepath.Join(home, ".zprofile"), filepath.Join(home, ".zshrc")}, true, nil
}

func configureEnvironment(scope, runner, explicit string, dry bool, out io.Writer) error {
	if scope != "runner" && scope != "user" && scope != "system" {
		return errors.New("--scope must be runner, user, or system")
	}
	if (scope == "runner") != (runner != "") {
		return errors.New("--runner-path is required for runner scope and invalid for other scopes")
	}
	dir, err := defaultCacheDir(explicit, scope)
	if err != nil {
		return err
	}
	if runner != "" {
		runner, err = filepath.Abs(runner)
		if err != nil {
			return err
		}
		if err := safeDirectory(runner, false); err != nil {
			return fmt.Errorf("runner path: %w", err)
		}
		listener := filepath.Join(runner, "bin", "Runner.Listener")
		if runtime.GOOS == "windows" {
			listener += ".exe"
		}
		if info, err := os.Stat(listener); err != nil || !info.Mode().IsRegular() {
			return fmt.Errorf("runner path %q must contain bin/Runner.Listener (or .exe on Windows)", runner)
		}
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return err
	}
	if scope == "user" && runtime.GOOS != "windows" {
		shell := filepath.Base(os.Getenv("SHELL"))
		if shell != "." && shell != "" && shell != "bash" && shell != "zsh" && shell != "sh" && shell != "dash" {
			return fmt.Errorf("user shell %q is unsupported; use runner scope or configure its environment explicitly", shell)
		}
	}
	files, shell, err := configurationFiles(runtime.GOOS, scope, home, runner)
	if err != nil {
		return err
	}
	type change struct {
		path string
		data []byte
		mode os.FileMode
	}
	var changes []change
	for _, p := range files {
		if fixtureConfigRoot != "" && scope != "runner" {
			p = filepath.Join(fixtureConfigRoot, strings.ReplaceAll(strings.TrimLeft(filepath.ToSlash(p), "/"), ":", ""))
		}
		mode := os.FileMode(0o644)
		var before []byte
		info, err := os.Lstat(p)
		if err == nil {
			if !info.Mode().IsRegular() {
				return fmt.Errorf("refusing nonregular configuration file %q", p)
			}
			mode = info.Mode().Perm()
			before, err = os.ReadFile(p)
		}
		if err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
		value := dir
		if !shell && scope != "runner" {
			if strings.ContainsAny(value, "\"\\$`") {
				return errors.New("Linux /etc/environment cache path cannot contain quotes, backslashes, dollar signs, or backticks")
			}
			value = "\"" + value + "\""
		}
		changes = append(changes, change{p, updateVariable(before, value, shell), mode})
	}
	if err := safeDirectory(dir, !dry); err != nil && !(dry && errors.Is(err, os.ErrNotExist)) {
		return fmt.Errorf("cache directory: %w", err)
	}
	if runtime.GOOS == "windows" && scope != "runner" {
		if err := persistRegistry(scope, dir, dry); err != nil {
			return err
		}
		fmt.Fprintf(out, "configured %s environment %s=%q dry-run=%t\n", scope, cacheVariable, dir, dry)
	}
	for _, ch := range changes {
		fmt.Fprintf(out, "configure %q: %s=%q dry-run=%t\n", ch.path, cacheVariable, dir, dry)
		if dry {
			continue
		}
		if fixtureConfigRoot != "" {
			if err := os.MkdirAll(filepath.Dir(ch.path), 0o755); err != nil {
				return err
			}
		}
		if err := atomicWrite(ch.path, ch.data, ch.mode); err != nil {
			return fmt.Errorf("persist environment in %q (check permissions/elevation; earlier listed files may already be updated): %w", ch.path, err)
		}
	}
	fmt.Fprintln(out, "Activation: restart the affected runner; reopen shells or log in again. Existing processes are unchanged.")
	fmt.Fprintln(out, "Ensure the runner account can read this cache. User/system login settings are not a universal service environment; use runner scope for services.")
	return nil
}
