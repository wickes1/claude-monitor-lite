package main

/*
#cgo CFLAGS: -x objective-c
#cgo LDFLAGS: -framework Cocoa
#include <stdlib.h>
#import <Cocoa/Cocoa.h>

// systray keeps its NSStatusItem in a private ivar of the app delegate; KVC
// reads ivars directly, so this needs no fork of the library.
static void setStatusItemAutosaveName(char *name) {
	NSString *s = [NSString stringWithUTF8String:name];
	free(name);
	dispatch_async(dispatch_get_main_queue(), ^{
		NSStatusItem *item = [(NSObject *)[NSApp delegate] valueForKey:@"statusItem"];
		item.autosaveName = s;
	});
}
*/
import "C"

// setStatusItemName gives this instance's menu bar item its own autosave name.
// systray leaves the default "Item-0", and a bare binary's defaults domain is
// its executable name, so every instance shared one saved position and one
// identity (process name + window title) in menu bar managers like Ice — two
// instances running at once fought over placement and Ice mixed them up.
func setStatusItemName() {
	name := "claude-monitor-lite"
	if id := instanceID(); id != "" {
		name += "." + id
	}
	C.setStatusItemAutosaveName(C.CString(name))
}
