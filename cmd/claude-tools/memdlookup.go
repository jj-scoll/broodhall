package main

// The guards can attach what a memory daemon knows about a file to Read and Edit calls.
// This build has no daemon, so every lookup is empty.

func memdLookupFile(cwd, file string, subagent bool) string { return "" }

func memdShellLookup() bool { return false }

func memdToolLookup(tool string, input map[string]any, cwd string, subagent bool) string {
	return ""
}
