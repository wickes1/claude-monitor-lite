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

// setStatusItemName gives the menu bar item its own autosave name. systray
// leaves the default "Item-0", which the item would share with every other
// systray app whose defaults domain is a bare executable name; under its own
// name macOS and menu bar managers like Ice keep its position and identity.
func setStatusItemName() {
	C.setStatusItemAutosaveName(C.CString("claude-monitor-lite"))
}
