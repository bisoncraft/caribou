// This code is available on the terms of the project LICENSE.md file,
// also available online at https://blueoakcouncil.org/license/1.0.0.

//go:build cgo && !windows

package main

import webview "github.com/bisoncraft/webview_go"

// Monitor selection and window placement are Windows-only. Elsewhere the
// window is sized from the display size and the window manager places it.

func launchMonitor() uintptr { return 0 }

func cursorMonitor() uintptr { return 0 }

func placeWindow(webview.WebView, uintptr) bool {
	return false
}
