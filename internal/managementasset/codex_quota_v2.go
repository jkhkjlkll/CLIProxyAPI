package managementasset

import _ "embed"

//go:embed codex_quota_v2.html
var codexQuotaV2HTML []byte

// CodexQuotaV2HTML returns the embedded Codex quota management page HTML.
func CodexQuotaV2HTML() []byte {
	return codexQuotaV2HTML
}
