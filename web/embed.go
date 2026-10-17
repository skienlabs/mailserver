// Package web embeds the Skien Mails single-page app.
package web

import "embed"

// Static holds the files served at "/".
//
//go:embed static
var Static embed.FS
