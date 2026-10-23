package hostfs

import "golang.org/x/sys/unix"

func renameExclusive(directory int, source, target string) error {
	return unix.Renameat2(directory, source, directory, target, unix.RENAME_NOREPLACE)
}
