[![GoDoc](https://pkg.go.dev/badge/github.com/deploymenttheory/go-sdk-winmediafoundry)](https://pkg.go.dev/github.com/deploymenttheory/go-sdk-winmediafoundry)
[![License](https://img.shields.io/github/license/deploymenttheory/go-sdk-winmediafoundry)](LICENSE)
[![Go Version](https://img.shields.io/github/go-mod/go-version/deploymenttheory/go-sdk-winmediafoundry)](https://go.dev/)
[![Release](https://img.shields.io/github/v/release/deploymenttheory/go-sdk-winmediafoundry)](https://github.com/deploymenttheory/go-sdk-winmediafoundry/releases)
[![codecov](https://codecov.io/gh/deploymenttheory/go-sdk-winmediafoundry/graph/badge.svg)](https://codecov.io/gh/deploymenttheory/go-sdk-winmediafoundry)
![Status: beta](https://img.shields.io/badge/status-beta-blue)

## Overview

A pure-Go, cross-platform toolkit for **acquiring and building Windows
installation media** — with no external tools (no wimlib, DISM,
oscdimg, or cabextract). It provides four Go library areas plus a CLI:

- **Windows Update service client** (`windowsuup/`) — makes direct SOAP calls to
  `fe3.delivery.mp.microsoft.com` / `fe3cr.delivery.mp.microsoft.com` to discover
  Windows builds by ring and architecture, resolve pre-signed CDN URLs, stream
  ESD/CAB files to disk, and diff file sets between builds.
- **ESD catalog client** (`esd/`) — a standalone client (same architecture as
  `windowsuup`) that fetches Microsoft's Media Creation Tool catalog
  (`products.cab`) and resolves full installation-ESD download URLs.
- **Consumer software-download client** (`softwaredownload/`) — a standalone
  client (same architecture as `windowsuup`) that reproduces Microsoft's
  consumer Windows 11 ISO download flow: it scrapes the public software-download
  pages, drives the download-connector handshake to resolve a signed,
  time-limited ISO link for a chosen edition and language, and streams the ISO
  to disk.
- **Windows imaging libraries** (`pkg/`) — read, extract, and write WIM/ESD
  images (LZMS / XPRESS / LZX), read CAB archives, write UDF file systems, and
  master bootable ISO9660 + El Torito images, culminating in a one-call
  ESD → bootable ISO builder.
- **Command-line tool** (`cli/`, `winmediafoundry`) — a Cobra/Viper CLI over all
  of the above. See [Command-line tool](#command-line-tool).

## Prerequisites

- Go 1.25 or later
- For the Windows Update client: outbound HTTPS (port 443) to
  `fe3.delivery.mp.microsoft.com` and `fe3cr.delivery.mp.microsoft.com` (no
  certificates required — the Microsoft CA chain is handled for you). The `pkg/`
  imaging libraries work entirely offline.

## Installation

```bash
go get github.com/deploymenttheory/go-sdk-winmediafoundry
```

## Usage

Runnable examples are in the `examples/` directory:

| Example | Description |
|---|---|
| `examples/01_fetch_builds` | Discover available Windows builds by ring and architecture |
| `examples/02_get_files` | Resolve pre-signed CDN download URLs for a build's files |
| `examples/03_download` | Stream ESD/CAB files concurrently to a local directory |
| `examples/04_diff` | Compare file sets between two builds |
| `examples/05_esd_catalog` | List the Media Creation Tool ESD catalog |
| `examples/06_wim_info` | Print a WIM/ESD's header and image list |
| `examples/07_wim_tree` | List an image's directory tree |
| `examples/08_wim_extract` | Extract an image's files to a directory |
| `examples/09_esd_to_iso` | Build a bootable ISO from an ESD |

Run any example directly:

```bash
go run ./examples/01_fetch_builds
```

## Command-line tool

A Cobra/Viper CLI in `cli/` exposes the whole toolkit:

```bash
go build -o winmediafoundry ./cli
```

| Command | Description |
|---|---|
| `builds` | List Windows Update builds |
| `files` | List a build's files (`--cdn-urls` to resolve download URLs) |
| `download` | Download a build's files to a directory |
| `diff --base --target` | Compare two builds |
| `esd catalog` | List the Media Creation Tool ESD catalog |
| `swdl list \| resolve \| download` | List, resolve, and download consumer Windows 11 ISOs |
| `wim info \| tree \| extract <file>` | Inspect or extract a WIM/ESD image |
| `iso build <esd> <out.iso>` | Build a bootable ISO from an ESD |

Configuration is layered **flags > environment > config file**. Global settings
may come from `$HOME/.winmediafoundry.yaml`, variables prefixed
`WINMEDIAFOUNDRY_` (e.g. `WINMEDIAFOUNDRY_ARCH=arm64`,
`WINMEDIAFOUNDRY_LOG_LEVEL=debug`), or flags (`--arch`, `--ring`, `--sku`,
`--timeout`, `--log-level`).

```bash
winmediafoundry esd catalog --edition Professional --architecture x64 --language en-us
winmediafoundry wim info ./install.esd
winmediafoundry iso build ./install.esd ./Windows.iso --label MY_WIN
```

## Creating a Client

```go
import "github.com/deploymenttheory/go-sdk-winmediafoundry/windowsuup"

client, err := windowsuup.NewClient()
if err != nil {
    log.Fatal(err)
}
```

`NewClient` accepts zero or more `ClientOption` values:

| Option | Type | Default | Description |
|---|---|---|---|
| `WithTimeout(d)` | `time.Duration` | 2 min | Per-SOAP-request HTTP timeout; CDN downloads are exempt |
| `WithTLSConfig(cfg)` | `*tls.Config` | embedded Microsoft CA bundle + system roots | Custom TLS configuration for SOAP connections |
| `WithHTTPClient(hc)` | `*http.Client` | internal | Replace the underlying HTTP client for SOAP calls; overrides `WithTimeout` and `WithTLSConfig` |
| `WithLogger(l)` | `*zap.Logger` | `zap.NewProduction()` | Structured logger |

Example with options:

```go
import (
    "time"
    "go.uber.org/zap"

    "github.com/deploymenttheory/go-sdk-winmediafoundry/windowsuup"
)

logger, _ := zap.NewDevelopment()
client, err := windowsuup.NewClient(
    windowsuup.WithTimeout(60 * time.Second),
    windowsuup.WithLogger(logger),
)
```

## Calling SDK Functions

### Fetch Builds

`client.Builds.FetchBuilds` calls the SyncUpdates SOAP endpoint and returns all available builds matching the given filters.

```go
import (
    buildsapi "github.com/deploymenttheory/go-sdk-winmediafoundry/windowsuup/api/builds"
    "github.com/deploymenttheory/go-sdk-winmediafoundry/windowsuup/constants"
)

builds, _, err := client.Builds.FetchBuilds(ctx,
    buildsapi.WithArch(constants.ArchAMD64),
    buildsapi.WithRing(constants.RingRetail),
    buildsapi.WithSKU(constants.SKUPro),
)
```

`FetchOption` reference:

| Option | Default | Description |
|---|---|---|
| `WithArch(arch)` | `ArchAMD64` | Target CPU architecture |
| `WithRing(ring)` | `RingRetail` | Windows Update release channel |
| `WithSKU(sku)` | `SKUPro` | Windows edition SKU |
| `WithFlight(flight)` | `"Active"` | Update flight sub-channel |
| `WithCheckBuild(build)` | `""` (SDK default) | OS version the client claims to run; an old value causes WU to offer the current release as an upgrade |
| `WithBuild(build)` | `""` | Filter to a specific build version string, e.g. `"26100.4061"` |

Each `models.Build` in the result includes `UUID`, `Revision`, `Title`, `Build` (version string), `Arch`, `Ring`, `IsStable`, and `IsInsider`.

### Get Files

`client.Files.GetFiles` retrieves the file list for a build. Without `WithCDNURLs`, it returns file metadata only (SHA1, SHA256, size). With `WithCDNURLs`, it calls GetExtendedUpdateInfo2 to resolve pre-signed Microsoft CDN download URLs that expire approximately 12 minutes after resolution.

```go
import (
    filesapi "github.com/deploymenttheory/go-sdk-winmediafoundry/windowsuup/api/files"
    "github.com/deploymenttheory/go-sdk-winmediafoundry/windowsuup/constants"
)

files, _, err := client.Files.GetFiles(ctx, build,
    filesapi.WithCDNURLs(),
    filesapi.WithLanguage("en-us"),
    filesapi.WithEdition(constants.EditionProfessional),
    filesapi.WithExtension(".esd"),
)
```

`FileOption` reference:

| Option | Description |
|---|---|
| `WithCDNURLs()` | Resolve live pre-signed CDN download URLs (expire ~12 min) |
| `WithLanguage(lang)` | Filter by BCP-47 language tag, e.g. `"en-us"`. Neutral files are always included. |
| `WithEdition(ed)` | Filter by Windows edition using filename substring matching |
| `WithExtension(ext)` | Filter by file extension, e.g. `".esd"` or `".cab"` |

Each `models.File` in the result includes `Name`, `SizeBytes`, `SHA1`, `SHA256`, `FileType`, and — when `WithCDNURLs` is set — `URL` and `ExpiresAt`.

### Download Files

`client.Download.DownloadFile` streams a single file from its CDN URL to a destination directory. `client.Download.DownloadFiles` downloads multiple files concurrently. Files are written atomically (temp file → rename); files already present at the correct size are skipped.

Both methods require files with a populated `URL` field — call `GetFiles` with `WithCDNURLs()` first.

```go
// Single file
resp, err := client.Download.DownloadFile(ctx, files[0], "./downloads")

// Multiple files — concurrency=0 defaults to 4 parallel downloads
err := client.Download.DownloadFiles(ctx, files, "./downloads", 4)
```

### Diff Builds

`client.Diff.Diff` compares the file sets of two builds client-side. It fetches file metadata for both builds (no CDN URLs) and compares by SHA256, falling back to SHA1, then size. The result reports files added, removed, changed, and unchanged.

```go
d, _, err := client.Diff.Diff(ctx, buildA, buildB)
fmt.Printf("+%d -%d ~%d =%d\n",
    len(d.Added), len(d.Removed), len(d.Changed), d.Unchanged)
```

`models.BuildDiff` fields:

| Field | Type | Description |
|---|---|---|
| `BaseUUID` / `TargetUUID` | `string` | Build UUIDs being compared |
| `BaseBuild` / `TargetBuild` | `string` | Build version strings |
| `Added` | `[]models.File` | Files present in target but not in base |
| `Removed` | `[]models.File` | Files present in base but not in target |
| `Changed` | `[]models.FileDiff` | Files present in both but with differing content |
| `Unchanged` | `int` | Count of files identical in both builds |

## ESD Catalog Client

The standalone `esd` client (its own `NewClient`, structured like `windowsuup`)
fetches Microsoft's Media Creation Tool catalog (`products.cab`), decompresses it
(pure-Go LZX), and returns the list of full installation ESDs with direct CDN
URLs and SHA-1 hashes.

```go
import (
    "github.com/deploymenttheory/go-sdk-winmediafoundry/esd"
    esdapi "github.com/deploymenttheory/go-sdk-winmediafoundry/esd/api/esd"
)

client, _ := esd.NewClient()
cat, _, err := client.Catalog(ctx, esdapi.WithProduct(esdapi.Windows11))
pro := cat.Filter("Professional", "x64", "en-us")
fmt.Println(pro[0].FileName, pro[0].URL)
```

### End to end: ESD → bootable ISO

```go
// 1. Resolve and download an ESD (see ESD Catalog above), then:
err := builder.BuildISO("install.esd", "Windows.iso",
    builder.Options{VolumeID: "CCCOMA_X64FRE"})
```

See [Windows Imaging](#windows-imaging-pure-go) for the underlying `pkg/`
libraries.

## Consumer Software-Download Client

The standalone `softwaredownload` client (its own `NewClient`, structured like
`windowsuup`) reproduces Microsoft's consumer Windows 11 ISO download flow
server-side. It scrapes the public software-download pages to list the available
product editions, drives the download-connector handshake (session whitelisting,
SKU lookup) to resolve a signed, time-limited ISO link for a chosen edition and
language, and — with `WithDownloadDir` — streams the multi-GB ISO straight to
disk (atomic write, skip-if-already-present).

```go
import (
    "github.com/deploymenttheory/go-sdk-winmediafoundry/softwaredownload"
    sdapi "github.com/deploymenttheory/go-sdk-winmediafoundry/softwaredownload/api/softwaredownload"
    sdconst "github.com/deploymenttheory/go-sdk-winmediafoundry/softwaredownload/constants"
)

client, _ := softwaredownload.NewClient()

// List the available editions (x64 and Arm64).
products, _, err := client.List(ctx, sdapi.WithArch(sdconst.ArchARM64))

// Resolve and download an Arm64 ISO by name, with a terminal progress bar.
link, _, err := client.GetByName(ctx, "Arm64",
    sdapi.WithLanguage("en-US"),
    sdapi.WithDownloadDir("./out"),
    sdapi.WithProgress(nil),
)
fmt.Println(link.FileName, link.LocalPath)
```

`GetByID` resolves by product-edition id (as returned by `List`), `GetByName` by
a case-insensitive substring of the edition name. Without `WithDownloadDir` both
only resolve the signed `DownloadLink` (`URL`, `FileName`, `Arch`, `ExpiresAt`);
`Download` streams an already-resolved link. The same flow is exposed on the CLI
as `winmediafoundry swdl list | resolve | download`.

## Constants Reference

All constants live in `github.com/deploymenttheory/go-sdk-winmediafoundry/windowsuup/constants`.

### Architectures

| Constant | Value |
|---|---|
| `ArchAMD64` | `"amd64"` |
| `ArchX86` | `"x86"` |
| `ArchARM64` | `"arm64"` |

### Rings

| Constant | Channel |
|---|---|
| `RingRetail` | Stable / generally available |
| `RingReleasePreview` | Release Preview Insider |
| `RingBeta` | Beta Insider |
| `RingExperimental` | Dev Insider |
| `RingCanary` | Canary Insider (fastest-moving) |
| `RingMSIT` | Microsoft internal |

`RingDev` is a deprecated alias for `RingExperimental`.

### SKUs

| Constant | Name | ID |
|---|---|---|
| `SKUPro` | Professional | 48 |
| `SKUHome` | Home | 1 |
| `SKUHomeN` | Home N | 2 |
| `SKUEnterprise` | Enterprise | 4 |
| `SKUEducation` | Education | 121 |
| `SKUProWorkstation` | Pro for Workstations | 161 |
| `SKUIoTEnterprise` | IoT Enterprise | 188 |
| `SKUServerStandard` | Server Standard | 7 |
| `SKUServerDatacenter` | Server Datacenter | 8 |

### Editions

Used with `filesapi.WithEdition(ed)` to filter files by Windows edition.

| Constant | Edition |
|---|---|
| `EditionHome` | Home (CORE) |
| `EditionHomeN` | Home N |
| `EditionProfessional` | Professional |
| `EditionProfessionalN` | Professional N |
| `EditionEnterprise` | Enterprise |
| `EditionEnterpriseN` | Enterprise N |
| `EditionEducation` | Education |
| `EditionEducationN` | Education N |
| `EditionProWorkstation` | Pro for Workstations |
| `EditionServerStandard` | Server Standard |
| `EditionServerDatacenter` | Server Datacenter |

## Windows Imaging (pure Go)

The `pkg/` directory holds standalone, cross-platform Windows-imaging libraries
with **no cgo and no external tools** — no wimlib, DISM, oscdimg, cabextract, or
genisoimage. They turn a downloaded ESD into a bootable installation ISO entirely
in Go.

| Package | Description |
|---|---|
| `pkg/wim` | Read, extract, and write WIM/ESD images: container, blob/offset table, XML catalog, solid LZMS resources, dentry tree, extraction, and a multi-image WIM writer |
| `pkg/wim/lzms` | LZMS decompressor (the ESD solid-resource format) |
| `pkg/wim/xpress` | XPRESS (LZ77 + Huffman) decompressor |
| `pkg/cab` | Microsoft Cabinet (`.cab`) reader with LZX and MSZIP decompression |
| `pkg/udf` | UDF 1.02 (ECMA-167) writer — the file system Windows install media uses |
| `pkg/iso` | Bootable ISO9660 + El Torito mastering, and the UDF + El Torito bridge master |
| `pkg/builder` | End-to-end ESD → bootable ISO orchestration |

LZX (for both CAB and WIM) reuses `github.com/Microsoft/go-winio/wim/lzx`;
ISO9660 framing uses `github.com/diskfs/go-diskfs`.

### Build a bootable ISO from an ESD

```go
import "github.com/deploymenttheory/go-sdk-winmediafoundry/pkg/builder"

err := builder.BuildISO("install.esd", "Windows.iso",
    builder.Options{VolumeID: "CCCOMA_X64FRE"})
```

This extracts the "Windows Setup Media" skeleton, rebuilds `sources/boot.wim` and
`sources/install.wim` from the ESD's images, and masters a UDF + El Torito ISO
that boots on both BIOS and UEFI. Because the media uses UDF, install images
larger than the ISO9660 4 GiB-per-file limit are handled natively.

See `examples/09_esd_to_iso`. To inspect or extract images without building an
ISO, use `pkg/wim` directly (`examples/06_wim_info`, `07_wim_tree`,
`08_wim_extract`).

## Package Layout

The Windows Update **service client** lives under `windowsuup/`, the standalone
**ESD catalog client** under `esd/`, the standalone **consumer software-download
client** under `softwaredownload/`, and the reusable **imaging libraries** under
`pkg/` (see above).

| Package | Description |
|---|---|
| `windowsuup` | WU service entry point — `Client`, `NewClient`, `ClientOption` |
| `windowsuup/api/builds` | `FetchBuilds` — build discovery via SyncUpdates SOAP |
| `windowsuup/api/files` | `GetFiles` — file metadata and CDN URL resolution via GetExtendedUpdateInfo2 |
| `windowsuup/api/download` | `DownloadFile` / `DownloadFiles` — streaming CDN downloads |
| `windowsuup/api/diff` | `Diff` — client-side file-set comparison |
| `windowsuup/constants` | `Arch`, `Ring`, `SKU`, `Edition` constants |
| `windowsuup/shared/models` | `Build`, `File`, `BuildDiff`, `FileDiff` types |
| `windowsuup/client` | Transport interface and concrete `Transport` implementation |
| `pkg/wuproto`, `pkg/wuproto/soap` | WU SOAP protocol types and client (GetCookie → SyncUpdates → GetExtendedUpdateInfo2) |
| `esd` | ESD catalog entry point — `Client`, `NewClient` (self-contained: own `client`, `shared/models`, `mocks`) |
| `esd/api/esd` | `Catalog` — fetch + parse the Media Creation Tool `products.cab` |
| `softwaredownload` | Consumer software-download entry point — `Client`, `NewClient` (self-contained: own `client`, `shared/models`, `mocks`) |
| `softwaredownload/api/softwaredownload` | `Get` / `List` / `GetByID` / `GetByName` / `Download` — scrape, resolve, and stream consumer Windows 11 ISOs |
| `cli`, `cli/cmd` | Cobra/Viper CLI (`winmediafoundry`) over the WU client, ESD client, software-download client, and `pkg/` libraries |

## Contributing

1. Fork the repository
2. Create a feature branch (`git checkout -b feature/my-change`)
3. Commit your changes
4. Open a pull request against `main`

All submissions must pass `go build ./...` and `go vet ./...`.

## License

MIT License. See [LICENSE](LICENSE) for details.

## Disclaimer

This project is an independent implementation. It is not affiliated with, endorsed by, or supported by Microsoft. Use in accordance with Microsoft's acceptable use policies.
