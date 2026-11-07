# Tetrate releases

This repository publishes the release files of Tetrate binaries and an index of every release. It holds no source code.

## Install tetrate

```sh
curl -fsSL https://get.tetrate.io | sh
```

The installer picks the right file for darwin-arm64, linux-amd64 or linux-arm64 and checks it. Run it with `sh -s -- --help` for the options.

## What is here

| Path | Content |
| --- | --- |
| `products.json` | The products this repository publishes, the repository each one is built in and the identity that signs it. |
| `<product>/index.json` | Every published version of the product, newest first, and the newest stable release and prerelease. |
| `<product>/<version>.json` | The manifest of one release: the source repository, tag and commit, the signer, and each file with its platform, size, sha256, download URL and signature bundle. A manifest is written once and never changed. |
| Releases tagged `<product>/<version>` | The files of that release: the binaries, `checksums.txt` and one sigstore bundle (`.sigstore.json`) for each file. |

Read the indexes from `https://raw.githubusercontent.com/tetrateio/releases/main/<product>/index.json`. They do not need the GitHub API.

## Verify a download

Every file is signed with sigstore by the release workflow of the repository it was built in. The files here are the same bytes as that release, so the signatures still verify. The manifest of each release names the exact signer. For `tetrate` v0.1.0-alpha.3:

```sh
cosign verify-blob \
  --bundle tetrate-v0.1.0-alpha.3-darwin-arm64.sigstore.json \
  --certificate-identity https://github.com/tetrateio/cli/.github/workflows/release.yml@refs/tags/v0.1.0-alpha.3 \
  --certificate-oidc-issuer https://token.actions.githubusercontent.com \
  --certificate-github-workflow-repository tetrateio/cli \
  --certificate-github-workflow-ref refs/tags/v0.1.0-alpha.3 \
  --certificate-github-workflow-trigger push \
  tetrate-v0.1.0-alpha.3-darwin-arm64
```

Then check the file against `checksums.txt`.

## How releases arrive

A release is built and signed in the product's own repository first. A maintainer then promotes it here with `tools/promote`, which downloads the release, verifies every file against the signer in `products.json` and refuses on any failure. It creates the release, writes the manifest and the index, and commits them.

## Promote a release

A maintainer promotes a release on purpose, after trying it from the private source. In a clean checkout of `main`, with `gh` logged in, `cosign` on `PATH` and a signing key for git:

```sh
go run ./tools/promote -product tetrate -tag v0.1.0-alpha.3 -dry-run
go run ./tools/promote -product tetrate -tag v0.1.0-alpha.3
```

The tool downloads the release from the source repository and verifies every file. It creates the release here as a draft with the same files and `LICENSE`, commits the manifest and the index, and publishes the release on that commit. Then it downloads each binary anonymously and checks its sha256. It refuses to rewrite a manifest that exists.

## License

The binaries are licensed under the Tetrate binary license. See [LICENSE](LICENSE).
