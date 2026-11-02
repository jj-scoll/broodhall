package main

// Client-specific rule data and private tool names. This build knows no client buckets, so
// raw-client-data asks only for Python reading local parquet files, and names no tools.

import "regexp"

// clientDataS3RE matches S3 objects that hold raw client data; nil means there are none.
var clientDataS3RE *regexp.Regexp

// Tools the guard messages point the model at; empty when this build has none.
const (
	structureTool  = "" // large-read-guard: file structure
	maskedCopyTool = "" // raw-client-data: masked parquet copy
)
