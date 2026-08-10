//go:build unix

package main

import (
	"fmt"
	"os"
	"syscall"
)

// preserveOwner makes path owned by the same uid/gid as ref.
//
// reindex replaces the index by rename, so the new file keeps the *temp* file's
// owner rather than the original's. That is not cosmetic: reindex is run as
// root (a one-off container) against files the server writes as nobody, and a
// root-owned .idx costs the running ingester every append with EACCES - reads
// keep working, so the archive looks healthy while silently recording nothing.
//
// Failing to chown is an error rather than a warning, for the same reason: the
// damage is invisible until someone notices the archive stopped growing.
func preserveOwner(path string, ref os.FileInfo) error {
	st, ok := ref.Sys().(*syscall.Stat_t)
	if !ok {
		return nil
	}
	uid, gid := int(st.Uid), int(st.Gid)

	cur, err := os.Stat(path)
	if err != nil {
		return err
	}
	if cst, ok := cur.Sys().(*syscall.Stat_t); ok &&
		int(cst.Uid) == uid && int(cst.Gid) == gid {
		return nil // already correct; no privilege needed
	}
	if err := os.Chown(path, uid, gid); err != nil {
		return fmt.Errorf("chown %s to %d:%d: %w", path, uid, gid, err)
	}
	return nil
}
