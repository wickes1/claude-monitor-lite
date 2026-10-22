package main

import (
	"encoding/xml"
	"io"
	"os"
	"reflect"
	"strconv"
	"strings"
	"testing"
)

// renderLaunchAgent must embed the label, the binary path followed by the
// "start" argument, and RunAtLoad — these are what make launchd run the
// monitor at login. The ProgramArguments order matters: launchd execs the
// first element with the rest as args, so the binary must precede "start".
func TestRenderLaunchAgent(t *testing.T) {
	const bin = "/opt/homebrew/bin/claude-monitor-lite"
	out := renderLaunchAgent(bin, "")

	wantContains := []string{
		"<string>" + launchAgentLabel() + "</string>",
		"<string>" + bin + "</string>",
		"<string>start</string>",
		"<key>RunAtLoad</key>",
		"<true/>",
	}
	for _, w := range wantContains {
		if !strings.Contains(out, w) {
			t.Errorf("renderLaunchAgent output missing %q", w)
		}
	}

	// The daemon rebinds its own log, so launchd-level log redirection would
	// only capture the launcher — these keys are deliberately absent.
	for _, absent := range []string{"StandardOutPath", "StandardErrorPath"} {
		if strings.Contains(out, absent) {
			t.Errorf("plist must NOT contain %s (the daemon owns its own log)", absent)
		}
	}

	// ProgramArguments order: the binary path must come before "start".
	bi := strings.Index(out, "<string>"+bin+"</string>")
	si := strings.Index(out, "<string>start</string>")
	if bi < 0 || si < 0 || bi > si {
		t.Errorf("ProgramArguments must be [binary, start] in order (binary@%d, start@%d)", bi, si)
	}

	// Anti: no KeepAlive — start forks a daemon then exits 0, so KeepAlive
	// would make launchd respawn the monitor after an intentional stop.
	if strings.Contains(out, "KeepAlive") {
		t.Error("plist must NOT contain KeepAlive")
	}
}

// A path with XML metacharacters (possible in a home directory name) must not
// produce a malformed plist.
func TestXMLEscape(t *testing.T) {
	cases := map[string]string{
		"plain":          "plain",
		"a&b":            "a&amp;b",
		"a<b>c":          "a&lt;b&gt;c",
		"/Users/A&B/bin": "/Users/A&amp;B/bin",
	}
	for in, want := range cases {
		if got := xmlEscape(in); got != want {
			t.Errorf("xmlEscape(%q) = %q, want %q", in, got, want)
		}
	}
}

// A home directory containing an ampersand must round-trip through
// renderLaunchAgent as escaped XML, not raw.
func TestRenderLaunchAgentEscapesPath(t *testing.T) {
	out := renderLaunchAgent("/Users/a&b/bin/cml", "")
	if strings.Contains(out, "a&b") {
		t.Error("renderLaunchAgent must escape & in the binary path")
	}
	if !strings.Contains(out, "a&amp;b") {
		t.Error("renderLaunchAgent should contain the escaped path")
	}
}

// serviceTarget (used by bootout) and domainTarget (used by bootstrap) must
// address the same launchd domain, or install and uninstall would silently
// target different services. This pins gui/<uid> + the label wiring so a
// gui->user swap, dropped uid, trailing slash, or label drift fails the build.
func TestLaunchctlTargetsShareDomain(t *testing.T) {
	wantDomain := "gui/" + strconv.Itoa(os.Getuid())
	if got := domainTarget(); got != wantDomain {
		t.Errorf("domainTarget() = %q, want %q", got, wantDomain)
	}
	if got, want := serviceTarget(), domainTarget()+"/"+launchAgentLabel(); got != want {
		t.Errorf("serviceTarget() = %q, want %q (must be domain + label)", got, want)
	}
}

// ProgramArguments must be EXACTLY [binary, "start"]. A flag slipped before
// "start" or a trailing arg would make launchd exec an unknown subcommand
// (main.go default -> "Unknown command" -> exit 1), silently breaking login
// autostart while the looser ordering check still passes.
func TestRenderLaunchAgentExactArgv(t *testing.T) {
	const bin = "/opt/homebrew/bin/claude-monitor-lite"
	got := programArguments(t, renderLaunchAgent(bin, ""))
	want := []string{bin, "start"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("ProgramArguments = %v, want %v", got, want)
	}
}

// The rendered plist must be well-formed XML for every path we escape, or
// launchd silently ignores a corrupt LaunchAgent — this encodes the
// parseability property the escaping tests assume.
func TestRenderLaunchAgentWellFormed(t *testing.T) {
	for _, p := range []string{"/opt/homebrew/bin/claude-monitor-lite", "/Users/a&b/bin/cml", "/x<y>z/cml"} {
		dec := xml.NewDecoder(strings.NewReader(renderLaunchAgent(p, "")))
		for {
			_, err := dec.Token()
			if err == io.EOF {
				break
			}
			if err != nil {
				t.Errorf("renderLaunchAgent(%q) is not well-formed XML: %v", p, err)
				break
			}
		}
	}
}

// programArguments extracts the <string> entries inside the ProgramArguments
// <array> by walking the plist's XML tokens (the only <array> in the plist).
func programArguments(t *testing.T, plist string) []string {
	t.Helper()
	dec := xml.NewDecoder(strings.NewReader(plist))
	var args []string
	inArray, capture := false, false
	for {
		tok, err := dec.Token()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatalf("plist not parseable: %v", err)
		}
		switch el := tok.(type) {
		case xml.StartElement:
			switch el.Name.Local {
			case "array":
				inArray = true
			case "string":
				capture = inArray
			}
		case xml.EndElement:
			switch el.Name.Local {
			case "array":
				inArray = false
			case "string":
				capture = false
			}
		case xml.CharData:
			if capture {
				args = append(args, string(el))
			}
		}
	}
	return args
}

// launchd starts jobs without the installing shell's environment, so a
// CLAUDE_CONFIG_DIR instance's plist must carry the variable itself — without
// it autostart silently monitors the default account. Each instance also needs
// its own label, or installing one boots out the other.
func TestRenderLaunchAgentInstance(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", "")
	defaultLabel := launchAgentLabel()
	if strings.Contains(renderLaunchAgent("/bin/cml", ""), "EnvironmentVariables") {
		t.Error("default instance plist must not set EnvironmentVariables")
	}

	const dir = "/Users/x/.claude-work"
	t.Setenv("CLAUDE_CONFIG_DIR", dir)
	if launchAgentLabel() == defaultLabel {
		t.Errorf("instance label %q must differ from the default label", launchAgentLabel())
	}
	out := renderLaunchAgent("/bin/cml", dir)
	for _, w := range []string{"<key>CLAUDE_CONFIG_DIR</key>", "<string>" + dir + "</string>", "<string>" + launchAgentLabel() + "</string>"} {
		if !strings.Contains(out, w) {
			t.Errorf("instance plist missing %q", w)
		}
	}
	if got := programArguments(t, out); !reflect.DeepEqual(got, []string{"/bin/cml", "start"}) {
		t.Errorf("ProgramArguments = %v, want [/bin/cml start]", got)
	}
}
