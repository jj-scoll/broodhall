// Command gitleaksrules turns the default rules of the gitleaks library into the data the
// guardrails binary evaluates (cmd/guardrails/secretrules), and proves that the detector there
// returns what the library returns. It is the only code in this repository that imports
// gitleaks, which is why it is a module of its own: the guardrails binary must not link it.
//
//	sh regen.sh [TAG]    everything below, in order; see regen.sh
//	go run . generate    write ../../cmd/guardrails/secretrules/rules_gen.go
//	go run . parity ...  run the library and the detector over the same inputs
//
// The library is used exactly as the guardrails binary used it before the rules were vendored:
// the default config, generic-api-key removed, MaxDecodeDepth 0, IgnoreGitleaksAllow true.
package main

import (
	"fmt"
	"os"
)

func main() {
	if len(os.Args) < 2 {
		usage()
	}
	var err error
	switch os.Args[1] {
	case "generate":
		err = generate(os.Args[2:])
	case "parity":
		err = parity(os.Args[2:])
	default:
		usage()
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "gitleaksrules:", err)
		os.Exit(1)
	}
}

func usage() {
	fmt.Fprintln(os.Stderr, "usage: gitleaksrules generate [-out FILE] [-toml FILE] | parity [-examples FILE] [-fixtures FILE] [-repo DIR] [-write]")
	os.Exit(2)
}
