### Added

- `arcli scheduler cq reload` refreshes all active continuous queries, and `arcli scheduler retention trigger` runs all active retention policies with a confirmation prompt. Both preserve Arc's admin, licence, disabled-scheduler, and role-gating errors. Closes [arcli#33](https://github.com/Basekick-Labs/arcli/issues/33).

- `arcli license status` inspects the public `/health` license block on standalone and clustered Arc deployments, with table and JSON output. Reports tier, server status, source, and optional expiry, days remaining, and site-license state. Expiry uses UTC RFC3339 with fractional seconds when provided.
- Absent/null license blocks succeed as normal OSS behavior and produce `{"configured":false,"license":null}` in JSON. Present licenses produce `{"configured":true,"license":{...}}`, preserving active, expired, invalid, unlicensed, and unknown server statuses. Optional absent/null fields are omitted; explicit zero days and false site-license values are retained.
- All successfully read license states exit 0. Configuration, request, decoding, and output failures exit nonzero. No token or database header is sent to `/health`, and no authentication verification is performed. Normal connection selection, timeout, TLS verification, and identity opt-out settings apply.

License installation and key/file configuration remain server responsibilities. Existing `ping -o json` license pass-through is unchanged. Closes [arcli#34](https://github.com/Basekick-Labs/arcli/issues/34).
