# Local candidate binaries

Run from the source root, with the Go version declared in `go.mod`:

```sh
python3 scripts/build_candidate.py /absolute/path/outside-source/new-candidate
```

The output directory must not exist. The command validates dependency notices,
then builds CGO-free binaries for macOS ARM64 and Linux AMD64/ARM64. Builds use
`-trimpath`, disable implicit Git metadata, and embed a `candidate-` version derived
from the hashed local build inputs. The command checks binary target metadata and
runs the version command on the host-compatible binary. Cross-built executables
still require runtime acceptance on their target platforms.

`manifest.json` records toolchain version, local package source, embedded assets,
license and notice input hashes, platform, size and SHA-256 for each binary.
The output includes root `LICENSE` and `NOTICE`, the full `LICENSES` directory,
and the accompanying dependency, toolchain, container and asset source records.
Keep these files alongside the executables. `SHA256SUMS` includes every emitted
binary, notice and the manifest, using paths relative to the output directory.
Inputs are re-read after building; any observed change discards the
staged result. No files are uploaded and no existing output directory is replaced.

These are internal verification artifacts, not public release packages. Commit and
build time deliberately remain `unknown` until a real release identity exists.
Checksums detect corruption; they are not signatures or trusted build provenance.
A public distribution still requires complete licenses/notices, security gates,
immutable source identity, hosted CI, signed provenance and release approval.

For a built container, `python3 scripts/container_sbom.py IMAGE_ID NEW_OUTPUT_DIR`
generates CycloneDX and Syft inventories plus an image archive and checksum receipt.
Use the immutable ID returned by `docker image inspect`, not a tag. The script binds
that ID to the archive's platform config digest; they may be different identifiers.

## Secret checks without registry access

The default `python3 scripts/security_scan.py` fetches the fixed scanner digests
before running network-disabled scans. After those exact images have been fetched,
`python3 scripts/security_scan.py --offline` can use the local cache. It verifies
repository digests, prevents implicit Docker pulls, and runs the same positive
controls and candidate checks. Missing or mismatched images cause failure; offline
mode does not skip any scanner or add exclusions. Hosted CI retains the default
image-fetching mode.
