### Added

- arcli rbac manages Enterprise organizations, teams, roles, measurement-specific permissions, and token team memberships. Lists support table, JSON, and CSV output; destructive changes prompt for confirmation. Token commands accept an id or exact name. Closes [arcli#27](https://github.com/Basekick-Labs/arcli/issues/27).
- `arcli license status` inspects the public `/health` license block on standalone and clustered Arc deployments, with table and JSON output. Reports tier, server status, source, and optional expiry, days remaining, and site-license state. Expiry uses UTC RFC3339 with fractional seconds when provided.
- Absent/null license blocks succeed as normal OSS behavior and produce `{"configured":false,"license":null}` in JSON. Present licenses produce `{"configured":true,"license":{...}}`, preserving active, expired, invalid, unlicensed, and unknown server statuses. Optional absent/null fields are omitted; explicit zero days and false site-license values are retained.
- All successfully read license states exit 0. Configuration, request, decoding, and output failures exit nonzero. No token or database header is sent to `/health`, and no authentication verification is performed. Normal connection selection, timeout, TLS verification, and identity opt-out settings apply.

License installation and key/file configuration remain server responsibilities. Existing `ping -o json` license pass-through is unchanged. Closes [arcli#34](https://github.com/Basekick-Labs/arcli/issues/34).
