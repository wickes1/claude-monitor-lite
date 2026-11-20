package main

/*
#cgo CFLAGS: -x objective-c
#cgo LDFLAGS: -framework Cocoa
#include <stdlib.h>
#import <Cocoa/Cocoa.h>

static NSString *takeString(char *s) {
	NSString *out = [NSString stringWithUTF8String:s];
	free(s);
	return out;
}

static NSTextField *aboutLabel(NSString *text) {
	NSTextField *field = [NSTextField labelWithString:text];
	field.font = [NSFont systemFontOfSize:NSFont.smallSystemFontSize];
	field.textColor = NSColor.secondaryLabelColor;
	return field;
}

static NSTextField *aboutValue(NSString *text) {
	NSTextField *field = [NSTextField wrappingLabelWithString:text];
	field.font = [NSFont systemFontOfSize:NSFont.smallSystemFontSize];
	// Paths and the version are there to be copied into an issue.
	field.selectable = YES;
	field.preferredMaxLayoutWidth = 300;
	return field;
}

static void onMainRunLoop(void (^block)(void)) {
	CFRunLoopPerformBlock(CFRunLoopGetMain(), kCFRunLoopCommonModes, block);
	CFRunLoopWakeUp(CFRunLoopGetMain());
}

// bringForward puts the window above the other apps' windows. A menu bar
// app is not the active app when its menu is used, and asking to be activated
// is not always granted, so without ordering the window front itself it opens
// behind whatever the user is working in — a modal window nobody can see.
static void bringForward(NSWindow *window) {
	[NSApp activateIgnoringOtherApps:YES];
	[window orderFrontRegardless];
	[window makeKeyWindow];
}

// showAboutPanel shows the About window: the standard alert layout (icon,
// name, one line on what the app does) over a grid of facts. rows holds one
// "label\tvalue" per line; a value's own line breaks are vertical tabs.
static void showAboutPanel(char *cTitle, char *cSummary, char *cRows, char *cRepoURL, char *cNotesURL) {
	NSString *title = takeString(cTitle), *summary = takeString(cSummary), *rows = takeString(cRows);
	NSString *repoURL = takeString(cRepoURL), *notesURL = takeString(cNotesURL);

	// Run from the run loop rather than the main dispatch queue: that queue is
	// serial, so a block that sits in a modal session holds up every block
	// queued after it — the one below that brings the window forward, and
	// anything else in the process — until the window is closed.
	onMainRunLoop(^{
		// A second click while the window is up brings it forward rather than
		// stacking another modal session on the first.
		static NSWindow *open = nil;
		if (open != nil) {
			bringForward(open);
			return;
		}

		NSAlert *alert = [[NSAlert alloc] init];
		alert.messageText = title;
		alert.informativeText = summary;
		// A bare binary has no bundle icon, and the generic executable icon
		// would stand in for it.
		NSImage *icon = [NSImage imageWithSystemSymbolName:@"gauge.with.needle" accessibilityDescription:nil];
		if (icon == nil) {
			icon = [NSImage imageWithSystemSymbolName:@"speedometer" accessibilityDescription:nil];
		}
		if (icon != nil) {
			alert.icon = [icon imageWithSymbolConfiguration:
				[NSImageSymbolConfiguration configurationWithPointSize:44 weight:NSFontWeightRegular]];
		}

		NSMutableArray<NSArray<NSView *> *> *cells = [NSMutableArray array];
		for (NSString *row in [rows componentsSeparatedByString:@"\n"]) {
			NSArray<NSString *> *parts = [row componentsSeparatedByString:@"\t"];
			if (parts.count != 2) {
				continue;
			}
			NSString *value = [parts[1] stringByReplacingOccurrencesOfString:@"\v" withString:@"\n"];
			[cells addObject:@[aboutLabel(parts[0]), aboutValue(value)]];
		}
		NSGridView *grid = [NSGridView gridViewWithViews:cells];
		grid.rowSpacing = 7;
		grid.columnSpacing = 10;
		grid.rowAlignment = NSGridRowAlignmentFirstBaseline;
		[grid columnAtIndex:0].xPlacement = NSGridCellPlacementTrailing;
		NSSize size = grid.fittingSize;
		grid.frame = NSMakeRect(0, 0, size.width, size.height);
		alert.accessoryView = grid;

		[alert addButtonWithTitle:@"OK"];
		[alert addButtonWithTitle:@"View on GitHub"];
		[alert addButtonWithTitle:@"Release Notes"];

		// Runs inside the modal session below, once it has put the window up.
		open = alert.window;
		onMainRunLoop(^{
			bringForward(open);
		});
		NSModalResponse response = [alert runModal];
		open = nil;
		NSString *url = response == NSAlertSecondButtonReturn ? repoURL
			: response == NSAlertThirdButtonReturn ? notesURL : nil;
		if (url != nil) {
			[NSWorkspace.sharedWorkspace openURL:[NSURL URLWithString:url]];
		}
	});
}
*/
import "C"

import "strings"

// showAboutPanel shows the About window. It returns at once; the window is
// run on the main thread.
func showAboutPanel(title, summary string, rows [][2]string, repoURL, notesURL string) {
	lines := make([]string, len(rows))
	for i, row := range rows {
		lines[i] = row[0] + "\t" + strings.ReplaceAll(row[1], "\n", "\v")
	}
	C.showAboutPanel(C.CString(title), C.CString(summary), C.CString(strings.Join(lines, "\n")),
		C.CString(repoURL), C.CString(notesURL))
}
