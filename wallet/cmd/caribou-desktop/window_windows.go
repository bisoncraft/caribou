// This code is available on the terms of the project LICENSE.md file,
// also available online at https://blueoakcouncil.org/license/1.0.0.

//go:build cgo

package main

/*
#include <windows.h>
#include <stdint.h>

typedef UINT (WINAPI *get_dpi_for_window_t)(HWND);

// Not in the SDK headers. When the shell starts a program it sets this flag,
// leaves STARTF_USESTDHANDLES clear, and puts the monitor it launched from
// in hStdOutput; the window manager uses that monitor for the program's first
// CW_USEDEFAULT window. With SEE_MASK_ICON the same field holds an icon
// instead, which GetMonitorInfo rejects.
#define CARIBOU_STARTF_SHELLPRIVATE 0x400

static int monitor_info(HMONITOR mon, MONITORINFO *mi) {
	mi->cbSize = sizeof(*mi);
	return mon != NULL && GetMonitorInfoW(mon, mi);
}

static uintptr_t cursor_monitor(void) {
	POINT pt;
	if (!GetCursorPos(&pt)) {
		return 0;
	}
	return (uintptr_t)MonitorFromPoint(pt, MONITOR_DEFAULTTONULL);
}

static uintptr_t launch_monitor(void) {
	STARTUPINFOW si;
	MONITORINFO mi;
	si.cb = sizeof(si);
	GetStartupInfoW(&si);
	if ((si.dwFlags & CARIBOU_STARTF_SHELLPRIVATE) && !(si.dwFlags & STARTF_USESTDHANDLES) &&
		monitor_info((HMONITOR)si.hStdOutput, &mi)) {
		return (uintptr_t)si.hStdOutput;
	}
	return cursor_monitor();
}

static UINT window_dpi(HWND h) {
	get_dpi_for_window_t getDpi = (get_dpi_for_window_t)(void *)GetProcAddress(
		GetModuleHandleW(L"user32.dll"), "GetDpiForWindow");
	UINT dpi = getDpi ? getDpi(h) : 0;
	return dpi ? dpi : 96;
}

typedef BOOL (WINAPI *adjust_window_rect_ex_for_dpi_t)(LPRECT, DWORD, BOOL, DWORD, UINT);

// adjust_window_rect grows a client rectangle to the outer window rectangle
// for the window's style, at the given DPI where the system supports it
// (Windows 10 1607 and later) and at the system DPI otherwise.
static void adjust_window_rect(HWND h, RECT *r, UINT dpi) {
	DWORD style = (DWORD)GetWindowLongPtrW(h, GWL_STYLE);
	DWORD exStyle = (DWORD)GetWindowLongPtrW(h, GWL_EXSTYLE);
	adjust_window_rect_ex_for_dpi_t adjust = (adjust_window_rect_ex_for_dpi_t)(void *)GetProcAddress(
		GetModuleHandleW(L"user32.dll"), "AdjustWindowRectExForDpi");
	if (adjust) {
		adjust(r, style, FALSE, exStyle, dpi);
	} else {
		AdjustWindowRectEx(r, style, FALSE, exStyle);
	}
}

// place_window puts the window on the given monitor (or, if that is not a
// valid monitor, the one it is on), sized to 80% of the monitor's work area,
// capped at 1920x1080 at the monitor's scale, and centered. webview makes the
// process per-monitor DPI aware, so the work area and SetWindowPos are both in
// physical pixels.
static int place_window(void *hwnd, uintptr_t monitor) {
	HWND h = (HWND)hwnd;
	HMONITOR target = (HMONITOR)monitor;
	MONITORINFO mi;
	if (!monitor_info(target, &mi)) {
		target = MonitorFromWindow(h, MONITOR_DEFAULTTONEAREST);
		if (!monitor_info(target, &mi)) {
			return 0;
		}
	}
	RECT wa = mi.rcWork;

	// A window crossing to a monitor with a different scale gets
	// WM_DPICHANGED, and webview answers it by rescaling the window. So move
	// it there first, shrunk so it lands wholly inside the work area, and size
	// it only once it is on its monitor and at that monitor's scale.
	if (MonitorFromWindow(h, MONITOR_DEFAULTTONEAREST) != target) {
		SetWindowPos(h, NULL, wa.left, wa.top, 1, 1, SWP_NOZORDER | SWP_NOACTIVATE);
	}
	UINT dpi = window_dpi(h);

	int areaW = wa.right - wa.left, areaH = wa.bottom - wa.top;
	int width = areaW * 4 / 5, height = areaH * 4 / 5;
	int maxW = MulDiv(1920, dpi, 96), maxH = MulDiv(1080, dpi, 96);
	if (width > maxW) {
		width = maxW;
	}
	if (height > maxH) {
		height = maxH;
	}
	// The UI's layout floor is 1024x600 CSS px of client area. 80% of a
	// 1920x1080 work area at 150% comes out just under that once the frame
	// is taken off, so the outer size is floored at the frame-inclusive
	// minimum, within the work area.
	RECT min = {0, 0, MulDiv(1024, dpi, 96), MulDiv(600, dpi, 96)};
	adjust_window_rect(h, &min, dpi);
	int minW = min.right - min.left, minH = min.bottom - min.top;
	if (width < minW) {
		width = minW < areaW ? minW : areaW;
	}
	if (height < minH) {
		height = minH < areaH ? minH : areaH;
	}
	return SetWindowPos(h, NULL, wa.left + (areaW - width) / 2, wa.top + (areaH - height) / 2,
		width, height, SWP_NOZORDER | SWP_NOACTIVATE) != 0;
}
*/
import "C"

import webview "github.com/bisoncraft/webview_go"

// launchMonitor returns the monitor this process was started from: the one
// the shell reports when it launched the program, otherwise the one under the
// mouse pointer. The handle is valid in other processes of the session, so it
// is passed to the window process and in window-open requests. 0 means
// unknown.
func launchMonitor() uintptr {
	return uintptr(C.launch_monitor())
}

// cursorMonitor returns the monitor under the mouse pointer, or 0.
func cursorMonitor() uintptr {
	return uintptr(C.cursor_monitor())
}

// placeWindow sizes the window and centers it on the given monitor, or on the
// monitor it opened on when that is 0 or no longer valid. webview's SetSize
// takes 96-DPI units and cannot position the window, which otherwise opens at
// the system's default cascade position. It returns false if no monitor's
// work area can be read, leaving the caller's sizing in place.
func placeWindow(w webview.WebView, monitor uintptr) bool {
	return C.place_window(w.Window(), C.uintptr_t(monitor)) != 0
}
