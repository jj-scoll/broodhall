package main

// Client-specific rule data. This build knows no client buckets, so raw-client-data asks
// only for Python reading local parquet files.

import "regexp"

// clientDataS3RE matches S3 objects that hold raw client data; nil means there are none.
var clientDataS3RE *regexp.Regexp
