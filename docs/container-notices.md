# Additional container materials

The runtime container includes more than the Go application. Its builder is pinned
in `deploy/Dockerfile`; changing that digest requires reviewing these materials.

| Material | Source in the builder | Current evidence |
| --- | --- | --- |
| TLS trust bundle | `/etc/ssl/certs/ca-certificates.crt` | Alpine `ca-certificates-bundle` 20261020-r0; installed package metadata declares `MPL-2.0 AND MIT`. Pinned source archive SHA-512 verified against its APKBUILD; source URL, certdata notice and full MPL 2.0 text are in `LICENSES/container-ca`. |
| Time zone archive | `/usr/local/go/lib/time/zoneinfo.zip` | Go 1.26.10 time-data README states the archive is compiled from the IANA database, which IANA asserts is public domain. The verbatim README is preserved under `LICENSES/toolchain/go1.26.10/time/README`. |
| Go runtime and external modules | Linked into the application | See [runtime dependency notices](third-party-notices.md) and the version/hash inventories. |

The image copies the currently collected notices into `/usr/share/licenses/ferry`.
The CA source identity and license materials have been collected; their presence
does not complete the outstanding first-party provenance review or overall release review. The local candidate is not yet a licensed
public distribution. Do not infer complete licensing coverage from an SBOM, an
Alpine package metadata field, or a successful vulnerability scan.
