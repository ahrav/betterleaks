//go:build !linux

package main

import "errors"

func validateNoReplaceCommit() error {
	return errors.New("atomic no-replace corpus commit requires Linux renameat2")
}

func renameNoReplace(_, _ string) error {
	return validateNoReplaceCommit()
}
