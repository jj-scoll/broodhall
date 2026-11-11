package main

import (
	"runtime/debug"

	"github.com/zricethezav/gitleaks/v8/detect"
)

// skipRules are the default rules the guardrails binary leaves out. generic-api-key matches
// key = value pairs by name and entropy, which the key-name masking owns (it tags those
// [secret:tag]) and which is the main source of false positives on command lines.
var skipRules = map[string]bool{"generic-api-key": true}

type finding struct{ Rule, Secret string }

// library is the gitleaks detector configured as the guardrails binary used it before the
// rules were vendored (cmd/guardrails/secretgitleaks.go at v1.11.0).
func library() (func(string) []finding, error) {
	d, err := detect.NewDetectorDefaultConfig()
	if err != nil {
		return nil, err
	}
	for id := range skipRules {
		delete(d.Config.Rules, id)
	}
	d.MaxDecodeDepth = 0         // match the text as written
	d.IgnoreGitleaksAllow = true // a "gitleaks:allow" comment must not hide a secret
	return func(s string) []finding {
		var out []finding
		for _, f := range d.DetectString(s) {
			out = append(out, finding{f.RuleID, f.Secret})
		}
		return out
	}, nil
}

// libraryVersion is the gitleaks module version this tool was built against.
func libraryVersion() string {
	if bi, ok := debug.ReadBuildInfo(); ok {
		for _, m := range bi.Deps {
			if m.Path == "github.com/zricethezav/gitleaks/v8" {
				return m.Version
			}
		}
	}
	return "unknown"
}
