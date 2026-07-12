//go:build linux

package corpus

import (
	"io/fs"
	"syscall"
)

func metadataFor(info fs.FileInfo) metadata {
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return metadata{MTimeNS: info.ModTime().UnixNano()}
	}
	blocks := uint64(0)
	if stat.Blocks > 0 {
		blocks = uint64(stat.Blocks)
	}
	return metadata{
		Device:  uint64(stat.Dev),
		Inode:   stat.Ino,
		Links:   uint64(stat.Nlink),
		UID:     uint64(stat.Uid),
		GID:     uint64(stat.Gid),
		Blocks:  blocks,
		MTimeNS: int64(stat.Mtim.Sec)*1_000_000_000 + int64(stat.Mtim.Nsec),
		CTimeNS: int64(stat.Ctim.Sec)*1_000_000_000 + int64(stat.Ctim.Nsec),
	}
}
