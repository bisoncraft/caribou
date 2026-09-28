// This code is available on the terms of the project LICENSE.md file,
// also available online at https://blueoakcouncil.org/license/1.0.0.

//go:build darwin

package main

/*
#cgo CFLAGS: -x objective-c
#cgo LDFLAGS: -framework Cocoa
#import <Cocoa/Cocoa.h>

void installEditMenu() {
	NSApplication *app = [NSApplication sharedApplication];
	NSMenu *mainMenu = [app mainMenu];
	if (mainMenu == nil) {
		mainMenu = [[NSMenu alloc] initWithTitle:@""];
		[app setMainMenu:mainMenu];
	}

	NSMenuItem *editMenuItem = [[NSMenuItem alloc] initWithTitle:@"Edit" action:nil keyEquivalent:@""];
	NSMenu *editMenu = [[NSMenu alloc] initWithTitle:@"Edit"];
	[editMenu addItemWithTitle:@"Undo" action:@selector(undo:) keyEquivalent:@"z"];
	NSMenuItem *redoItem = [editMenu addItemWithTitle:@"Redo" action:@selector(redo:) keyEquivalent:@"Z"];
	[redoItem setKeyEquivalentModifierMask:NSEventModifierFlagCommand | NSEventModifierFlagShift];
	[editMenu addItem:[NSMenuItem separatorItem]];
	[editMenu addItemWithTitle:@"Cut" action:@selector(cut:) keyEquivalent:@"x"];
	[editMenu addItemWithTitle:@"Copy" action:@selector(copy:) keyEquivalent:@"c"];
	[editMenu addItemWithTitle:@"Paste" action:@selector(paste:) keyEquivalent:@"v"];
	[editMenu addItemWithTitle:@"Select All" action:@selector(selectAll:) keyEquivalent:@"a"];
	[editMenuItem setSubmenu:editMenu];
	[mainMenu addItem:editMenuItem];
}
*/
import "C"

// installEditMenu adds a standard macOS Edit menu (Undo/Redo/Cut/Copy/Paste/
// Select All) to the app's main menu. The webview_go library never creates
// one, and without it Cmd+V (and Cmd+C/Cmd+X/Cmd+A) have no menu item bound
// to the standard paste:/copy:/cut:/selectAll: selectors for the OS to route
// the key equivalent to — so paste silently does nothing in every input
// field in the embedded WKWebView, regardless of what the page's own
// JavaScript does. Must run after the webview (and so NSApplication) exists.
func installEditMenu() {
	C.installEditMenu()
}
