//go:build !linux

package corpus

import "io/fs"

func metadataFor(info fs.FileInfo) metadata {
	return metadata{MTimeNS: info.ModTime().UnixNano()}
}
