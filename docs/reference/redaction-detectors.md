# Redaction detector reference

Auto-generated from `internal/redact/redact.go:builtinDetectors` via `make docs-detectors`. The list reflects the production set Default() returns; new detectors land here automatically once they ship in the source.

Detectors are listed in registration order. On overlap, the first match wins — most specific / longest-prefix patterns are registered first by design (so e.g. an Anthropic key is tagged `anthropic_api_key`, not the looser `openai_api_key` it would also match). See [../explanation/threat-model.md](../explanation/threat-model.md) for the contract these enforce.

| # | Pattern | Regex |
|---|---|---|
| 1 | `anthropic_api_key` | `\bsk-ant-[A-Za-z0-9_-]{20,}\b` |
| 2 | `openai_api_key` | `\bsk-(?:proj-)?[A-Za-z0-9_-]{40,}\b` |
| 3 | `google_api_key` | `\bAIza[0-9A-Za-z_-]{35}\b` |
| 4 | `gcp_oauth_access_token` | `\bya29\.[A-Za-z0-9_-]{20,}` |
| 5 | `gcp_oauth_refresh_token` | `\b1//0[A-Za-z0-9_-]{40,}` |
| 6 | `gcp_oauth_client_secret` | `\bGOCSPX-[A-Za-z0-9_-]{20,}\b` |
| 7 | `github_pat_fine_grained` | `\bgithub_pat_[A-Za-z0-9_]{82}\b` |
| 8 | `gitlab_pat` | `\bglpat-[A-Za-z0-9_-]{20,}\b` |
| 9 | `huggingface_token` | `\bhf_[A-Za-z0-9]{30,}\b` |
| 10 | `slack_app_token` | `\bxapp-\d-[A-Z0-9]+-\d+-[a-f0-9]{20,}\b` |
| 11 | `sendgrid_api_key` | `\bSG\.[A-Za-z0-9_-]{22}\.[A-Za-z0-9_-]{43}\b` |
| 12 | `github_pat_classic` | `\bgh[pousr]_[A-Za-z0-9]{36}\b` |
| 13 | `aws_access_key` | `\bAKIA[0-9A-Z]{16}\b` |
| 14 | `npm_token` | `\bnpm_[A-Za-z0-9]{36}\b` |
| 15 | `slack_token` | `\bxox[abprs]-[0-9]{10,}-[0-9a-zA-Z-]{24,}\b` |
| 16 | `stripe_key` | `\b(?:sk\|pk\|rk)_(?:live\|test)_[A-Za-z0-9]{24,}\b` |
| 17 | `twilio_sid` | `\b(?:AC\|SK)[a-f0-9]{32}\b` |
| 18 | `pem_private_key` | `-----BEGIN (?:RSA \|EC \|OPENSSH \|PGP \|DSA \|ENCRYPTED )?PRIVATE KEY-----[\s\S]*?-----END [^-]*PRIVATE KEY-----` |
| 19 | `jwt` | `\beyJ[A-Za-z0-9_-]{10,}\.[A-Za-z0-9_-]{10,}\.[A-Za-z0-9_-]{10,}\b` |
| 20 | `bearer_token` | `(?i)\bbearer\s+[A-Za-z0-9._~+/=-]{20,}\b` |
| 21 | `db_connection_string` | `(?i)\b(?:postgres(?:ql)?\|mysql\|mongodb(?:\+srv)?\|redis(?:s)?\|amqp)://[^:@\s]+:[^@\s]+@[^\s]+` |
| 22 | `basic_auth_url` | `\bhttps?://[^:@\s/]+:[^@\s/]+@[^\s]+` |
| 23 | `aws_secret_key_assignment` | `(?i)\baws_?secret(?:_access)?_?key\s*[:=]\s*["']?[A-Za-z0-9/+=]{40}["']?` |
