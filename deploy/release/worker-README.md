# Ferry release receiver

The administrator installs a receiver bound to one repository ID and service directory. A dedicated SSH key can prepare, inspect, commit or roll back an exact release; it cannot open a shell. Keep application data and the installation key inaccessible to the release runner.

The release image is built and scanned in CI. The worker downloads and verifies that artifact, and never compiles source. Readiness checks the full commit and authentication boundaries. The second commit follows canonical HTTPS checks.

For an initial migration, an administrator can stage a verified `smart-api/sqlite-backup/v1` snapshot in `bootstrap/import`, with private regular files. The receiver restores only into an empty data directory and records the manifest digest; it refuses to overwrite initialized data or substitute a different snapshot. The application performs schema upgrades at startup. Keep the source backup until migration acceptance.

This Compose configuration starts a production instance, including account renewal and upstream background jobs. Before handing over copied OAuth grants, stop the source instance and stage its final consistent data. A parallel validation copy must explicitly add `-standby` to the app command; do not publish this production configuration over a validation copy while its source is still active. See [migration](../../docs/guide/migration.md). External backup timers and their destinations must be configured independently.

Host administrators can preview owned terminal copies with `ferry-receiver cleanup` and apply a fresh plan with `cleanup --apply`. Scheduled backups also run local retention before contacting S3. The release and restore locks protect pending operations and referenced recovery points. See [local cleanup](../../docs/guide/local-cleanup.md) for retention, receipts, and legacy copies that require reconciliation.
