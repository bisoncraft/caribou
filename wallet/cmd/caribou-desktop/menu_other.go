// This code is available on the terms of the project LICENSE.md file,
// also available online at https://blueoakcouncil.org/license/1.0.0.

//go:build cgo && !darwin

package main

// installEditMenu is a no-op outside darwin. Windows and Linux webview
// backends don't have this gap: GTK/WebKitGTK on Linux and the Edge
// WebView2 control on Windows both wire up standard clipboard shortcuts on
// their own.
func installEditMenu() {}
