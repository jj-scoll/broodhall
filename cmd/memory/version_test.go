package main

import (
	"bytes"
	"strings"
	"testing"
)

// A release build stamps the tag (see .github/workflows/release.yml); the MCP serverInfo and
// `memory version` both report it, without the leading v.
func TestMCPServerInfoVersionIsTheRelease(t *testing.T) {
	old := stampVersion
	stampVersion = "v1.10.4"
	defer func() { stampVersion = old }()
	resp := mcpRun(t, "/nonexistent/sock",
		`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-03-26","capabilities":{},"clientInfo":{"name":"t","version":"1"}}}`)
	info := resp[0]["result"].(map[string]any)["serverInfo"].(map[string]any)
	if info["name"] != "memory" || info["version"] != "1.10.4" {
		t.Errorf("serverInfo = %v, want name memory, version 1.10.4", info)
	}
}

func TestMemoryVersionCmd(t *testing.T) {
	old := stampVersion
	stampVersion = "v1.10.4"
	defer func() { stampVersion = old }()
	var out bytes.Buffer
	if rc := memVersionTo(&out); rc != 0 {
		t.Fatalf("rc = %d", rc)
	}
	if got := out.String(); !strings.HasPrefix(got, "1.10.4") || !strings.HasSuffix(got, "\n") {
		t.Errorf("memory version printed %q, want 1.10.4 first", got)
	}
	if _, ok := commands["version"]; !ok {
		t.Error("no version subcommand")
	}
}

func TestVersionString(t *testing.T) {
	for _, tt := range []struct {
		ver, rev string
		dirty    bool
		want     string
	}{
		{"1.10.4", "5b8331fabcdef", false, "1.10.4 (5b8331f)"},
		{"1.10.4", "", false, "1.10.4"},
		{"", "5b8331fabcdef", true, "5b8331f-dirty"},
		{"", "", false, "unknown"},
	} {
		if got := versionString(tt.ver, tt.rev, tt.dirty); got != tt.want {
			t.Errorf("versionString(%q,%q,%v) = %q, want %q", tt.ver, tt.rev, tt.dirty, got, tt.want)
		}
	}
}
