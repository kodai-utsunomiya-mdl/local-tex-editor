// Package branding supplies the application's icon to the Windows installer.
package branding

import _ "embed"

// Icon contains the same multi-resolution artwork used by the executable.
//
//go:embed app-icon.ico
var Icon []byte
