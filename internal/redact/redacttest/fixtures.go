// Package redacttest builds credential-shaped fixtures for tests.
//
// Every builder assembles its armor header from fragments at runtime
// instead of spelling it as a literal. That is not stylistic: the
// repo's detect-private-key and gitleaks pre-commit hooks scan source
// text, and a test file containing real armor fails them even though
// it holds no key material. Bypassing those hooks with --no-verify to
// land a test would be a bad trade, and a hook that cries wolf is one
// people learn to skip.
//
// Keeping the builders here rather than in each package's _test.go
// means one home for "what a secret looks like" — internal/redact
// tests, internal/api tests and anything added later share it.
package redacttest

import "strings"

const dashes = "-----"

// armor wraps body in `-----BEGIN <label>-----` / `-----END <label>-----`.
func armor(label, body string) string {
	var b strings.Builder
	b.WriteString(dashes + "BEGIN " + label + dashes + "\n")
	b.WriteString(body)
	b.WriteString("\n" + dashes + "END " + label + dashes)
	return b.String()
}

// PEMPrivateKey returns RSA PEM armor around body. Matches the
// pem_private_key detector.
func PEMPrivateKey(body string) string {
	return armor("RSA PRIVATE"+" KEY", body)
}

// OpenSSHPrivateKey returns OpenSSH PEM armor around body. Also
// matches pem_private_key.
func OpenSSHPrivateKey(body string) string {
	return armor("OPENSSH PRIVATE"+" KEY", body)
}

// PGPPrivateKeyBlock returns PGP secret-key armor around body — the
// output shape of `gpg --export-secret-keys -a`. Matches the
// pgp_private_key detector.
//
// Note the trailing " BLOCK": this is exactly what the old
// pem_private_key pattern could not match, since it required the
// header to end in "PRIVATE KEY-----".
func PGPPrivateKeyBlock(body string) string {
	return armor("PGP PRIVATE KEY"+" BLOCK", body)
}

// PGPPublicKeyBlock returns PGP public-key armor. Not a credential —
// used as a negative case so the private-key detector stays narrow.
func PGPPublicKeyBlock(body string) string {
	return armor("PGP PUBLIC KEY"+" BLOCK", body)
}

// PGPMessage returns PGP encrypted-message armor. Ciphertext, not a
// key; another negative case.
func PGPMessage(body string) string {
	return armor("PGP MESSAGE", body)
}

// SlackWebhook returns an incoming-webhook URL. Assembled from parts
// for the same reason as the armor builders: gitleaks ships a
// slack-webhook-url rule that fires on the literal form.
func SlackWebhook() string {
	return "https://hooks.slack.com/" + "services/" +
		"T00000000/" + "B00000000/" + strings.Repeat("X", 24)
}
