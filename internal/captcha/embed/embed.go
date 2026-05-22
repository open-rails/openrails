package embed

import _ "embed"

// ClientScriptTemplate is the provider-neutral captcha browser client template.
// Runtime config values are injected by the HTTP handler before serving it.
//
//go:embed captcha_client.js
var ClientScriptTemplate string
