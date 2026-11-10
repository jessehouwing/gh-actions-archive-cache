package app

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"unicode"

	lockfile "github.com/github/actions-lockfile/go/pkg/lockfile"
	"github.com/spf13/cobra"
)

// NewCommand constructs the extension's command tree.
func NewCommand(out, stderr io.Writer) *cobra.Command {
	root := &cobra.Command{
		Use: "gh-actions-archive-cache", Short: "Populate and maintain the GitHub Actions runner archive cache",
		SilenceErrors: true, SilenceUsage: true,
	}
	root.SetOut(out)
	root.SetErr(stderr)
	var cacheDir, format, host string
	var dry bool
	root.PersistentFlags().StringVar(&cacheDir, "cache-dir", "", "Cache directory (default: environment, then user cache)")
	root.PersistentFlags().StringVar(&format, "format", "auto", "Archive format: auto, zip, tar.gz, both")
	root.PersistentFlags().StringVar(&host, "hostname", "", "GitHub home host (github.com or tenant.ghe.com)")
	root.PersistentFlags().BoolVar(&dry, "dry-run", false, "Preview without writing or deleting files")

	for _, name := range []string{"download", "from-lockfile", "cleanup"} {
		name := name
		var majors, perMajor int
		var protect []string
		cmd := &cobra.Command{Use: name, Args: cobra.ExactArgs(1)}
		switch name {
		case "download":
			cmd.Use += " OWNER/REPO[/PATH][@REF]"
			cmd.Short = "Download selected stable versions or an exact ref"
		case "from-lockfile":
			cmd.Use += " [PATH]"
			cmd.Short = "Download every pinned lockfile dependency without resolving refs"
			cmd.Args = cobra.MaximumNArgs(1)
		case "cleanup":
			cmd.Use += " OWNER/REPO"
			cmd.Short = "Delete identified older version archives, preserving pins and unknown files"
			cmd.Flags().StringArrayVar(&protect, "lockfile", nil, "Additional lockfile whose pins must be retained (repeatable)")
		}
		if name != "from-lockfile" {
			cmd.Flags().IntVar(&majors, "latest-major-versions", 0, "Keep latest N majors (0: unlimited)")
			cmd.Flags().IntVar(&perMajor, "latest-versions-per-major", 0, "Keep latest N version tags per major plus major pointers (0: unlimited)")
		}
		cmd.RunE = func(cmd *cobra.Command, args []string) (result error) {
			if majors < 0 || perMajor < 0 {
				return errors.New("version limits must be nonnegative")
			}
			if name == "cleanup" && majors == 0 && perMajor == 0 {
				return errors.New("cleanup requires at least one positive retention limit")
			}
			if name == "download" {
				_, ref, err := actionInput(args[0])
				if err != nil {
					return err
				}
				if ref != "" && (cmd.Flags().Changed("latest-major-versions") || cmd.Flags().Changed("latest-versions-per-major")) {
					return errors.New("exact refs and version-limit flags are mutually exclusive")
				}
			}
			formats, err := archiveFormats(format)
			if err != nil {
				return err
			}
			g, err := newGitHub(host)
			if err != nil {
				return err
			}
			dir, err := defaultCacheDir(cacheDir, "user")
			if err != nil {
				return err
			}
			c, release, err := openCache(dir, dry)
			if err != nil {
				return err
			}
			defer func() { result = errors.Join(result, release()) }()
			var entries []entry
			switch name {
			case "download":
				entries, err = g.resolve(cmd.Context(), args[0], majors, perMajor, formats)
			case "from-lockfile":
				p := filepath.FromSlash(lockfile.Path)
				if len(args) > 0 {
					p = args[0]
				}
				entries, err = readLocked(g, p, formats)
			case "cleanup":
				repo, ref, parseErr := actionInput(args[0])
				if parseErr != nil {
					return parseErr
				}
				if ref != "" || strings.Count(args[0], "/") != 1 {
					return errors.New("cleanup requires OWNER/REPO, without a ref or subpath")
				}
				p := filepath.FromSlash(lockfile.Path)
				if _, statErr := os.Stat(p); statErr == nil {
					protect = append(protect, p)
				} else if !errors.Is(statErr, os.ErrNotExist) {
					return statErr
				}
				for _, path := range protect {
					pins, readErr := readLocked(g, path, []string{"zip", "tar.gz"})
					if readErr != nil {
						return readErr
					}
					entries = append(entries, pins...)
				}
				return c.cleanup(cmd.Context(), g, repo, majors, perMajor, formats, entries, dry, out, stderr)
			}
			if err != nil {
				return err
			}
			return c.download(cmd.Context(), g, entries, dry, out, stderr)
		}
		root.AddCommand(cmd)
	}
	var scope, runnerPath string
	configure := &cobra.Command{
		Use: "configure", Short: "Persist the cache environment variable for a runner, user, or system",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			return configureEnvironment(scope, runnerPath, cacheDir, dry, out)
		},
	}
	configure.Flags().StringVar(&scope, "scope", "", "Required: runner, user, or system")
	configure.Flags().StringVar(&runnerPath, "runner-path", "", "Runner installation directory (runner scope only)")
	root.AddCommand(configure)
	return root
}

func readLocked(g *github, p string, formats []string) ([]entry, error) {
	f, err := os.Open(p)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	b, err := io.ReadAll(io.LimitReader(f, lockfile.MaxParseSize+1))
	if err != nil {
		return nil, err
	}
	entries, err := g.locked(b, formats)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", p, err)
	}
	return entries, nil
}

func archiveFormats(value string) ([]string, error) {
	switch value {
	case "auto":
		if runtime.GOOS == "windows" {
			return []string{"zip"}, nil
		}
		return []string{"tar.gz"}, nil
	case "zip", "tar.gz":
		return []string{value}, nil
	case "both":
		return []string{"zip", "tar.gz"}, nil
	default:
		return nil, fmt.Errorf("invalid format %q: expected auto, zip, tar.gz, or both", value)
	}
}

func defaultCacheDir(explicit, scope string) (string, error) {
	dir := explicit
	if dir == "" {
		dir = os.Getenv("ACTIONS_RUNNER_ACTION_ARCHIVE_CACHE")
	}
	if dir == "" && scope == "system" {
		switch runtime.GOOS {
		case "windows":
			dir = `C:\actionarchivecache`
		case "linux":
			dir = "/opt/actionarchivecache"
		}
	}
	if dir == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", err
		}
		dir = filepath.Join(home, "actionarchivecache")
	}
	if strings.IndexFunc(dir, unicode.IsControl) >= 0 {
		return "", errors.New("cache path cannot contain control characters")
	}
	return filepath.Abs(dir)
}
