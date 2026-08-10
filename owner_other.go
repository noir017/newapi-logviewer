//go:build !unix

package main

import "os"

// preserveOwner is a no-op off unix: there is no uid/gid to carry across, and
// the deployment this matters for (a root-run reindex against files the server
// writes as nobody) only exists in the container.
func preserveOwner(path string, ref os.FileInfo) error { return nil }
