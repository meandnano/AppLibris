//go:build !unix

package main

// ownerUID has no answer off unix: there is no uid to report, so
// mkdirError says only which uid the process runs as.
func ownerUID(string) (int, bool) {
	return 0, false
}
