# fetch

[![CI](https://github.com/headlesslab/fetch/actions/workflows/ci.yml/badge.svg)](https://github.com/headlesslab/fetch/actions/workflows/ci.yml)
[![Go Reference](https://pkg.go.dev/badge/github.com/headlesslab/fetch.svg)](https://pkg.go.dev/github.com/headlesslab/fetch)

A generic zip archive fetcher: race several download hosts, verify the archive's SHA-256 before extraction, and land the extracted tree in one atomic rename.

`fetch` is a [Satellite module](https://github.com/headlesslab/wand/blob/main/CONTEXT.md#dependencies) of [wand](https://github.com/headlesslab/wand), whose launcher uses it to download managed browsers. It knows nothing about browsers: no URL templates, platform names or revisions. Those belong to the caller.

## Usage

```go
import "github.com/headlesslab/fetch"

err := fetch.Zip(ctx, "/home/me/.cache/app/tool-1.2.3", []string{
	"https://origin.example.com/tool/1.2.3/tool-linux64.zip",
	"https://mirror.example.net/tool/1.2.3/tool-linux64.zip",
},
	fetch.WithSHA256("9f86d081884c7d659a2feaa0c55ad015a3bf4f1b2b0b822cd15d6c15b0f00a08"),
	fetch.WithStripFirstDir(), // the archive nests everything in one top-level folder
	fetch.WithLogger(log.Default()),
)
```

`fetch.Zip(ctx, dir, urls, opts...)` fetches one archive into the directory `dir`:

- **Every URL is probed concurrently**; the first host to answer with the first bytes of the archive wins. The others stay in reserve, in the order they answered: when a download fails, or its hash does not match, the next responder is tried. When no URL yields a verified archive, the error names every attempt.
- **The archive is hashed while it downloads and compared with `WithSHA256` before it is opened.** A mismatch fails with a `*fetch.HashMismatchError` naming the expected and the actual digest. Without `WithSHA256` the archive is extracted unverified, and the logger says so.
- **A file lock (`<dir>.lock`, via [`gofrs/flock`](https://github.com/gofrs/flock)) is held around download and extraction**, so concurrent fetchers of the same directory, in one process or across processes, serialize; the ones that wait find the directory in place and return without downloading. The lock file stays on disk: removing it would race other waiters.
- **Extraction goes into a temporary sibling directory that is renamed to `dir` at the end**, so `dir` either does not exist or is complete. Any failure removes the temporary directory. Entries that would escape the destination (absolute paths, `..`, a symbolic link whose target leads outside once every earlier link is followed, an entry placed under a symbolic link) fail the fetch. File modes and symbolic links are preserved; on Windows, recreating a symbolic link needs a privilege or Developer Mode, so an archive with one fails there.
- `WithStripFirstDir` expects exactly one top-level folder in the archive and makes that folder's content `dir`, as browser archives nest one folder. Anything else at the top level fails the fetch rather than being dropped.
- When `dir` already exists, `Zip` returns without a request. Remove `dir` to fetch again.
- The context cancels probing, download and extraction. `WithClient` supplies the `*http.Client` (default `http.DefaultClient`); `WithLogger` receives one line per event: the host chosen, progress, verification, extraction.

Zip is the only archive format. Requires Go 1.21 or later.

## Development

The suite runs against ephemeral HTTP servers and needs no network:

```sh
go test -race ./...
```

CI runs through the reusable workflow in [`headlesslab/.github`](https://github.com/headlesslab/.github) on Linux (Go floor and stable), Windows and macOS, with golangci-lint and govulncheck.

## License

[MIT](LICENSE).
