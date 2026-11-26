// Command gomodsbundle downloads Go modules together with all of their
// dependencies (test dependencies included) into a fresh, isolated module
// cache and packs that cache's cache/download tree into a zip that can be
// unpacked straight into an offline GOPROXY.
//
// Because the module cache is isolated, nothing from your regular
// ~/go/pkg/mod ends up in the zip: only the requested modules and what
// they need.
package main

import (
	"archive/zip"
	"bufio"
	"bytes"
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"net/url"
	"os"
	"os/exec"
	"os/signal"
	"path"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

type options struct {
	out              string
	listFile         string
	reuseCache       bool
	noSumdb          bool
	includeToolchain bool
	keep             bool
	quiet            bool
}

const usageText = `gomodsbundle - bundle Go modules and all their dependencies for an offline GOPROXY

Usage:
  gomodsbundle [flags] module[@version] ...
  gomodsbundle [flags] -f modules.txt

Each module is resolved in its own throwaway project against an empty module
cache, so the zip holds exactly the requested modules plus everything they
need to build and test, in the layout of $GOMODCACHE/cache/download.
A missing version means @latest. Lines in -f files may also be pasted
go.mod require lines ("github.com/foo/bar v1.2.3 // indirect").

Examples:
  gomodsbundle github.com/spf13/cobra@v1.8.1 golang.org/x/sync@latest
  gomodsbundle -o deps.zip -f modules.txt

Flags:
`

func main() {
	var o options
	flag.StringVar(&o.out, "o", "", "output zip `file` (default gomodsbundle-<timestamp>.zip)")
	flag.StringVar(&o.listFile, "f", "", "read modules from `file`, one per line (\"-\" for stdin, # starts a comment)")
	flag.BoolVar(&o.reuseCache, "reuse-cache", false, "fetch from your existing module cache first, falling back to GOPROXY\n(saves bandwidth; @latest is then resolved against your local cache)")
	flag.BoolVar(&o.noSumdb, "no-sumdb", false, "leave the sumdb/ checksum-database cache out of the zip")
	flag.BoolVar(&o.includeToolchain, "include-toolchain", false, "keep golang.org/toolchain in the zip if Go had to download a newer toolchain")
	flag.BoolVar(&o.keep, "keep", false, "keep the temporary directory for inspection")
	flag.BoolVar(&o.quiet, "q", false, "hide the output of the go commands")
	flag.Usage = func() {
		fmt.Fprint(flag.CommandLine.Output(), usageText)
		flag.PrintDefaults()
	}
	flag.Parse()

	mods, err := collectModules(flag.Args(), o.listFile)
	if err != nil {
		fatal(err)
	}
	if len(mods) == 0 {
		flag.Usage()
		os.Exit(2)
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	err = run(ctx, mods, o)
	stop()
	if err != nil {
		fatal(err)
	}
}

func fatal(err error) {
	fmt.Fprintln(os.Stderr, "gomodsbundle:", err)
	os.Exit(1)
}

// collectModules gathers module queries from the command line and the
// optional list file, normalizes them and drops duplicates.
func collectModules(args []string, file string) ([]string, error) {
	raw := append([]string(nil), args...)
	if file != "" {
		var r io.Reader = os.Stdin
		if file != "-" {
			f, err := os.Open(file)
			if err != nil {
				return nil, err
			}
			defer f.Close()
			r = f
		}
		sc := bufio.NewScanner(r)
		for sc.Scan() {
			raw = append(raw, parseLine(sc.Text())...)
		}
		if err := sc.Err(); err != nil {
			return nil, fmt.Errorf("reading %s: %w", file, err)
		}
	}

	seen := make(map[string]bool)
	var mods []string
	for _, m := range raw {
		m = normalize(m)
		if m == "" || seen[m] {
			continue
		}
		seen[m] = true
		mods = append(mods, m)
	}
	return mods, nil
}

// parseLine turns one line of a list file into module queries. It accepts
// "path@version", bare paths, and go.mod style "path version" lines.
func parseLine(line string) []string {
	if i := strings.Index(line, "#"); i >= 0 {
		line = line[:i]
	}
	var toks []string
	for _, t := range strings.Fields(line) {
		if strings.HasPrefix(t, "//") { // go.mod comment, e.g. "// indirect"
			break
		}
		if t == "require" || t == "(" || t == ")" {
			continue
		}
		toks = append(toks, t)
	}
	if len(toks) == 2 && !strings.Contains(toks[0], "@") && isVersion(toks[1]) {
		return []string{toks[0] + "@" + toks[1]}
	}
	return toks
}

func isVersion(s string) bool {
	return len(s) > 1 && s[0] == 'v' && s[1] >= '0' && s[1] <= '9' && !strings.Contains(s, "/")
}

// normalize tolerates pasted URLs ("https://github.com/foo/bar.git") and
// defaults the version to @latest.
func normalize(s string) string {
	s = strings.TrimSpace(s)
	s = strings.TrimPrefix(s, "https://")
	s = strings.TrimPrefix(s, "http://")
	p, v, _ := strings.Cut(s, "@")
	p = strings.TrimSuffix(strings.TrimSuffix(p, "/"), ".git")
	if p == "" {
		return ""
	}
	if v == "" {
		v = "latest"
	}
	return p + "@" + v
}

func run(ctx context.Context, mods []string, o options) error {
	goBin, err := exec.LookPath("go")
	if err != nil {
		return errors.New("the go command was not found in PATH")
	}

	tmp, err := os.MkdirTemp("", "gomodsbundle-")
	if err != nil {
		return err
	}
	if o.keep {
		defer fmt.Fprintln(os.Stderr, "temporary directory kept at", tmp)
	} else {
		defer removeAll(tmp)
	}

	gopath := filepath.Join(tmp, "gopath")
	modcache := filepath.Join(gopath, "pkg", "mod")
	// Later entries win over the inherited ones (os/exec keeps the last
	// value of a duplicated key). GOPROXY, GOPRIVATE, GONOSUMDB, GOSUMDB,
	// GOTOOLCHAIN etc. are inherited from your environment / go env file.
	env := append(os.Environ(),
		"GOPATH="+gopath,
		"GOMODCACHE="+modcache,
		"GOFLAGS=-mod=mod -modcacherw",
		"GOWORK=off",
		"GO111MODULE=on",
	)

	if o.reuseCache {
		proxy, err := localCacheProxy(ctx, goBin)
		if err != nil {
			return fmt.Errorf("-reuse-cache: %w", err)
		}
		env = append(env, "GOPROXY="+proxy)
		fmt.Fprintln(os.Stderr, "using GOPROXY="+proxy)
	}

	var failed []string
	for i, m := range mods {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		fmt.Fprintf(os.Stderr, "==> [%d/%d] %s\n", i+1, len(mods), m)
		dir := filepath.Join(tmp, "work", strconv.Itoa(i))
		if err := fetch(ctx, goBin, env, dir, m, o.quiet); err != nil {
			fmt.Fprintf(os.Stderr, "    FAILED: %v\n", err)
			failed = append(failed, m)
		}
	}
	if len(failed) > 0 {
		return fmt.Errorf("%d module(s) failed, no zip written: %s", len(failed), strings.Join(failed, ", "))
	}

	if o.out == "" {
		o.out = "gomodsbundle-" + time.Now().Format("20060102-150405") + ".zip"
	}
	st, err := writeZip(o.out, filepath.Join(modcache, "cache", "download"), o)
	if err != nil {
		return fmt.Errorf("writing zip: %w", err)
	}
	fmt.Fprintf(os.Stderr, "wrote %s: %d module versions (%d with source), %d files, %s\n",
		o.out, st.mods, st.zips, st.files, humanBytes(st.bytes))
	return nil
}

// localCacheProxy builds a GOPROXY value that reads your existing module
// cache first and falls back to your normal GOPROXY for anything missing.
func localCacheProxy(ctx context.Context, goBin string) (string, error) {
	out, err := exec.CommandContext(ctx, goBin, "env", "GOMODCACHE", "GOPROXY").Output()
	if err != nil {
		return "", fmt.Errorf("go env: %w", err)
	}
	lines := strings.Split(strings.TrimSpace(string(out)), "\n")
	if len(lines) != 2 || strings.TrimSpace(lines[0]) == "" {
		return "", fmt.Errorf("unexpected go env output %q", out)
	}
	p := filepath.ToSlash(filepath.Join(strings.TrimSpace(lines[0]), "cache", "download"))
	if !strings.HasPrefix(p, "/") { // Windows: C:/... -> /C:/...
		p = "/" + p
	}
	u := (&url.URL{Scheme: "file", Path: p}).String()
	// "," falls through to the next proxy only on not-found, which is what
	// a missing file in the local cache produces.
	return u + "," + strings.TrimSpace(lines[1]), nil
}

// fetch resolves one module query in its own scratch project so every
// module gets its own dependency graph, exactly as a consumer would see it.
func fetch(ctx context.Context, goBin string, env []string, dir, mod string, quiet bool) error {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	modPath, _, _ := strings.Cut(mod, "@")

	steps := []struct {
		args     []string
		required bool
	}{
		{[]string{"mod", "init", "gomodsbundle.invalid/tmp"}, true},
		// Adds the module and downloads the .mod/.info of its whole graph.
		{[]string{"get", mod}, true},
		// Loads every package of the module plus its tests and their full
		// import graph; with -mod=mod any import not covered by go.mod
		// (untidy or pre-modules dependencies) is resolved and fetched too.
		{[]string{"list", "-e", "-deps", "-test", modPath + "/..."}, false},
		// Downloads source zips for every module in the build list, which
		// includes the test dependencies recorded in the module's go.mod.
		{[]string{"mod", "download", "all"}, true},
	}

	for _, s := range steps {
		if !quiet {
			fmt.Fprintf(os.Stderr, "    $ go %s\n", strings.Join(s.args, " "))
		}
		var errBuf bytes.Buffer
		cmd := exec.CommandContext(ctx, goBin, s.args...)
		cmd.Dir = dir
		cmd.Env = env
		cmd.Stdout = io.Discard
		if quiet {
			cmd.Stderr = &errBuf
		} else {
			cmd.Stderr = os.Stderr
		}
		if err := cmd.Run(); err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			msg := fmt.Errorf("go %s: %w", strings.Join(s.args, " "), err)
			if quiet && errBuf.Len() > 0 {
				msg = fmt.Errorf("%w\n%s", msg, strings.TrimSpace(errBuf.String()))
			}
			if s.required {
				return msg
			}
			fmt.Fprintf(os.Stderr, "    warning (continuing): %v\n", msg)
		}
	}
	return nil
}

type stats struct {
	mods, zips, files int
	bytes             int64
}

// writeZip packs the cache/download tree rooted at root into dst, leaving
// out lock and temporary files (and optionally sumdb / toolchains).
func writeZip(dst, root string, o options) (st stats, err error) {
	partial := dst + ".partial"
	f, err := os.Create(partial)
	if err != nil {
		return st, err
	}
	defer func() {
		if err != nil {
			f.Close()
			os.Remove(partial)
		}
	}()

	zw := zip.NewWriter(f)
	err = filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(root, p)
		if err != nil {
			return err
		}
		name := filepath.ToSlash(rel)
		if d.IsDir() {
			if (o.noSumdb && name == "sumdb") || (!o.includeToolchain && name == "golang.org/toolchain") {
				return filepath.SkipDir
			}
			return nil
		}
		if skipFile(name) {
			return nil
		}

		info, err := d.Info()
		if err != nil {
			return err
		}
		hdr, err := zip.FileInfoHeader(info)
		if err != nil {
			return err
		}
		hdr.Name = name
		hdr.Method = zip.Deflate
		if strings.HasSuffix(name, ".zip") { // already compressed
			hdr.Method = zip.Store
		}
		w, err := zw.CreateHeader(hdr)
		if err != nil {
			return err
		}
		var n int64
		if path.Base(name) == "list" && path.Base(path.Dir(name)) == "@v" && !strings.HasPrefix(name, "sumdb/") {
			data, err := bundledVersions(p)
			if err != nil {
				return err
			}
			m, err := w.Write(data)
			if err != nil {
				return err
			}
			n = int64(m)
		} else {
			src, err := os.Open(p)
			if err != nil {
				return err
			}
			n, err = io.Copy(w, src)
			src.Close()
			if err != nil {
				return err
			}
		}

		st.files++
		st.bytes += n
		if path.Base(path.Dir(name)) == "@v" && !strings.HasPrefix(name, "sumdb/") {
			switch path.Ext(name) {
			case ".mod":
				st.mods++
			case ".zip":
				st.zips++
			}
		}
		return nil
	})
	if err != nil {
		return st, err
	}
	if err = zw.Close(); err != nil {
		return st, err
	}
	if err = f.Close(); err != nil {
		return st, err
	}
	err = os.Rename(partial, dst)
	return st, err
}

// bundledVersions returns the contents of an @v/list file reduced to the
// versions whose source zip is in the bundle. The go command also fetches
// the .mod/.info of a module's latest version (to check for retractions)
// and records it in list; left there, an offline proxy would answer
// @latest with a version it cannot serve.
func bundledVersions(listFile string) ([]byte, error) {
	data, err := os.ReadFile(listFile)
	if err != nil {
		return nil, err
	}
	dir := filepath.Dir(listFile)
	var out bytes.Buffer
	for _, v := range strings.Fields(string(data)) {
		if _, err := os.Stat(filepath.Join(dir, v+".zip")); err == nil {
			out.WriteString(v + "\n")
		}
	}
	return out.Bytes(), nil
}

func skipFile(name string) bool {
	for _, suf := range []string{".lock", ".partial", ".tmp"} {
		if strings.HasSuffix(name, suf) {
			return true
		}
	}
	return false
}

// removeAll deletes dir even if parts of it were made read-only (the module
// cache and extracted toolchains are read-only by default).
func removeAll(dir string) {
	_ = filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err == nil && d.IsDir() {
			_ = os.Chmod(p, 0o755)
		}
		return nil
	})
	if err := os.RemoveAll(dir); err != nil {
		fmt.Fprintln(os.Stderr, "warning: could not remove temporary directory:", err)
	}
}

func humanBytes(n int64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	div, exp := int64(unit), 0
	for m := n / unit; m >= unit; m /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %ciB", float64(n)/float64(div), "KMGTPE"[exp])
}
