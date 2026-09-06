//go:build !linux && !darwin

package store

import "errors"

var errAtomicPublishUnsupported = errors.New("atomic non-overwriting backup publication is supported only on Linux and Darwin")

func renameExclusive(source, destination string) error {
	return errAtomicPublishUnsupported
}
