# Tetrate releases

This repository publishes the release files of Tetrate binaries and an index of every release. It holds no source code.

## Install tetrate

```sh
curl -fsSL https://get.tetrate.io | sh
```

The installer picks the right file for darwin-arm64, linux-amd64 or linux-arm64 and checks it. Run it with `sh -s -- --help` for the options.

On Debian, Ubuntu, Fedora and RHEL, install the `.deb` or `.rpm` package of a release whose manifest lists `packages`. Replace `vX` with the version, and `amd64` with `arm64` on an Arm host:

```sh
curl -fLO https://github.com/tetrateio/releases/releases/download/tetrate/vX/tetrate-vX-linux-amd64.deb
sudo apt install ./tetrate-vX-linux-amd64.deb
sudo dnf install https://github.com/tetrateio/releases/releases/download/tetrate/vX/tetrate-vX-linux-amd64.rpm
```

The packages are not in an apt or dnf repository and have no GPG signature. Verify a package with its sigstore bundle, as below, before you install it.

## What is here

| Path | Content |
| --- | --- |
| `products.json` | The products this repository publishes, the repository each one is built in, the identity that signs it and its Linux packages. |
| `<product>/index.json` | Every published version of the product, newest first, and the newest stable release and prerelease. |
| `<product>/<version>.json` | The manifest of one release: the source repository, tag and commit, the signer, and each file with its platform, size, sha256, download URL and signature bundle. Binaries are under `assets` and Linux packages under `packages`, each with its format. A manifest is written once and never changed. |
| Releases tagged `<product>/<version>` | The files of that release: the binaries, the Linux packages, `checksums.txt` and one sigstore bundle (`.sigstore.json`) for each file. |
| `keys/tetrate-packages.asc` | The public OpenPGP key `Tetrate packages`, kept for signing apt and dnf repository metadata. Nothing is signed with it here. |

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

A maintainer promotes a release on purpose, after trying it from the private source. For `tetrate`, the whole sequence of release, promotion, npm approval and Homebrew update is in `docs/releasing.md` of `tetrateio/cli`. In a clean checkout of `main`, with `gh` logged in, `cosign` on `PATH` and a signing key for git:

```sh
go run ./tools/promote -product tetrate -tag v0.1.0-alpha.3 -dry-run
go run ./tools/promote -product tetrate -tag v0.1.0-alpha.3
```

The tool downloads the release from the source repository and verifies every file. It creates the release here as a draft with the same files and `LICENSE`, commits the manifest and the index, and publishes the release on that commit. Then it downloads each binary and package anonymously and checks its sha256. It refuses to rewrite a manifest that exists.

## Update the Homebrew formula

After a promotion, write the formula of the newest release into a checkout of `tetrateio/homebrew-tap` and push it with a signed commit:

```sh
go run ./tools/formula -product tetrate -tap ../homebrew-tap -commit
```

The formula comes from the manifest, so it names the same URLs and sha256 digests. Users install with `brew install tetrateio/tap/tetrate`.

## Publish to npm

The `npm` workflow stages `@tetrate/cli`, its three platform packages and the unscoped `tetrate` when a `tetrate/v*` release is published, or by hand with a version. A staged version is not public until a maintainer approves it on npmjs.com, or with `npm stage list <package>` and `npm stage approve <stage-id>`, with 2FA. The run summary lists the staged packages. The trusted publisher of each package may only stage. `tools/npm` builds the packages from the public release files and checks each binary against the manifest and its sigstore bundle. npm trusted publishing needs no stored secret. Only the first publish of a new package uses a short-lived `NPM_TOKEN` secret in the `npm` environment, because npm accepts a trusted publisher only on a package that exists. Delete the secret after that publish. To build the packages locally:

```sh
go run ./tools/npm -product tetrate -out out/npm -cosign
```

`out/npm/publish.txt` lists the package directories in publish order with their dist-tag.

## Publish to PyPI

The `pypi` workflow publishes the `tetrate` wheels when a `tetrate/v*` release is published, or by hand with a version. `tools/wheel` builds one wheel per platform from the public release files, after it checks each binary against the manifest and its sigstore bundle. Each wheel holds only the binary, in its scripts directory, so `uvx tetrate`, `uv tool install tetrate`, `pipx install tetrate` and `pip install tetrate` install it into `bin` as it is. The version follows PEP 440: `v0.1.0-alpha.3` is `0.1.0a3`. The upload uses PyPI trusted publishing with attestations and runs in the `pypi` environment, which waits for a reviewer. To build the wheels locally:

```sh
go run ./tools/wheel -product tetrate -out out/pypi -cosign
```

## Package signing key

`keys/tetrate-packages.asc` is the public key `Tetrate packages <support@tetrate.io>`, fingerprint `4B08B03E4B1664D681ABAB2C05DD5B6BCB7C4791`. Its primary key only certifies. Its subkey `619A8C6E870F7BA1BE3F174FBECC436D664A70F6` signs. Both expire on 2028-11-06. The primary secret key, its passphrase and its revocation certificate are in a shared Tetrate 1Password vault. A maintainer who signs holds only the signing subkey. Nothing here is signed with it. `docs/proposals/linux-package-repositories.md` of `tetrateio/cli` proposes the apt and dnf repositories that would use it.

## License

The binaries are licensed under the Tetrate binary license. See [LICENSE](LICENSE).
