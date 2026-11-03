package main

import (
	"strings"
	"testing"
)

// This build has neither of the private tools, so no guard message may send the model to them.
// Names are split so they stay out of the public tree.
func TestGuardMessagesNameNoPrivateTools(t *testing.T) {
	for i, msg := range guardHintMessages(t) {
		if msg == "" {
			t.Errorf("message %d is empty", i)
		}
		for _, private := range []string{"smart-explore", "claude-" + "mem", "pii-" + "mirror"} {
			if strings.Contains(msg, private) {
				t.Errorf("message %d = %q names %s", i, msg, private)
			}
		}
	}
}
