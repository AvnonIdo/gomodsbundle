# gomodsbundle

Bundle Go modules plus all of their dependencies (test dependencies included)
into a zip laid out like `$GOMODCACHE/cache/download`, ready to unpack into an
offline GOPROXY.

## Install

```bash
go install github.com/AvnonIdo/gomodsbundle@latest
```

Or, from a clone:

```bash
go build -o gomodsbundle .
```

No third-party dependencies; Go 1.21+.

## Use

```bash
gomodsbundle github.com/spf13/cobra@v1.8.1 golang.org/x/sync
gomodsbundle -o deps.zip -f modules.txt
```

`modules.txt` takes one module per line. `#` starts a comment, a missing
version means `@latest`, and you can paste `require` lines from a go.mod
(`github.com/foo/bar v1.2.3 // indirect`) or URLs (`https://github.com/foo/bar`).

Unzip the result into the same place you used to copy the contents of
`~/go/pkg/mod/cache/download`.

| Flag | Meaning |
|---|---|
| `-o file` | output zip (default `gomodsbundle-<timestamp>.zip`) |
| `-f file` | read modules from a file (`-` = stdin) |
| `-reuse-cache` | read from your existing `~/go/pkg/mod` cache first and fall back to GOPROXY. Saves bandwidth; the zip still contains only what the requested modules need. `@latest` is then resolved against what you have locally. |
| `-no-sumdb` | leave the `sumdb/` checksum cache out of the zip |
| `-include-toolchain` | keep `golang.org/toolchain` if Go had to download a newer toolchain (left out by default; it's large and specific to one platform) |
| `-keep` | keep the temp directory for inspection |
| `-q` | hide the output of the go commands |

## How it works

1. It creates a temporary, empty `GOPATH`/`GOMODCACHE`, so nothing from your
   regular module cache leaks into the bundle.
2. For **each** requested module it makes its own throwaway project, so two
   versions of the same module, or modules with conflicting requirements,
   don't affect each other's dependency graph:
   - `go mod init`
   - `go get <module>@<version>`: adds the module and fetches the `.mod`/`.info`
     files of its module graph
   - `go list -e -deps -test <module>/...`: loads every package of the module
     **and its tests** and their imports. With `-mod=mod`, any import that the
     go.mod files don't cover (untidy or pre-modules dependencies) gets
     resolved and downloaded too.
   - `go mod download all`: fetches source zips for every module in the build
     list, which includes test-only dependencies
3. It zips `cache/download`, leaving out `*.lock`, `*.partial` and `*.tmp`
   files. If any module fails, no zip is written and the failed modules are
   listed.
4. Each `@v/list` file in the zip is trimmed to the versions whose source zip
   is included. `go get` also fetches the latest version's `.mod` to check
   for retractions, and records that version in `list`. Left in, the offline
   proxy would answer `@latest` with a version it can't serve.

**Unzipping several bundles into one proxy directory:** a newer bundle's
`@v/list` overwrites the older one for the same module. Exact-version requests
still work because every `.info`/`.mod`/`.zip` stays on disk, but `@latest` and
`go list -m -versions` only see the versions in the last bundle you unzipped.
To rebuild every `list` from what's on disk:

```bash
find . -path '*/@v/*.zip' | sed 's|/@v/\(.*\)\.zip$| \1|' | sort -u |
  awk '{f = $1 "/@v/list"; if (f != prev) { if (prev) close(prev); prev = f }; print $2 > f}'
```

Your `GOPROXY`, `GOPRIVATE`, `GONOSUMDB`, `GOSUMDB` and `GOTOOLCHAIN`
settings are respected. `GOFLAGS` is overridden for the run.
