//go:build linux

// Package sparse implements the extent wire format used to transfer
// Firecracker memory-backing files with holes preserved, so a mostly
// zero-filled guest memory snapshot costs network bytes only for the
// pages actually touched.
//
// Wire format (also documented in internal/api): repeated frames of
// [offset uint64 LE][length uint64 LE][length bytes of data] until
// EOF.
package sparse

import (
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"os"

	"golang.org/x/sys/unix"
)

// Extent is one contiguous allocated (non-hole) byte range of a
// file.
type Extent struct {
	Off int64
	Len int64
}

// WalkExtents returns the allocated byte ranges of f, in order,
// using SEEK_DATA/SEEK_HOLE. A file with no allocated data (e.g. all
// holes, or empty) yields no extents.
func WalkExtents(f *os.File) ([]Extent, error) {
	fd := int(f.Fd())
	size, err := f.Seek(0, io.SeekEnd)
	if err != nil {
		return nil, fmt.Errorf("seek end: %w", err)
	}
	if size == 0 {
		return nil, nil
	}

	var extents []Extent
	pos := int64(0)
	for pos < size {
		dataStart, err := unix.Seek(fd, pos, unix.SEEK_DATA)
		if err != nil {
			if errors.Is(err, unix.ENXIO) {
				break // no more data from pos to EOF
			}
			return nil, fmt.Errorf("seek data at %d: %w", pos, err)
		}
		holeStart, err := unix.Seek(fd, dataStart, unix.SEEK_HOLE)
		if err != nil {
			return nil, fmt.Errorf("seek hole at %d: %w", dataStart, err)
		}
		extents = append(extents, Extent{Off: dataStart, Len: holeStart - dataStart})
		pos = holeStart
	}
	return extents, nil
}

// StreamExtents writes extents (as returned by WalkExtents, sourced
// from f) to w in the contract wire format.
func StreamExtents(w io.Writer, f *os.File, extents []Extent) error {
	var hdr [16]byte
	for _, e := range extents {
		binary.LittleEndian.PutUint64(hdr[0:8], uint64(e.Off))
		binary.LittleEndian.PutUint64(hdr[8:16], uint64(e.Len))
		if _, err := w.Write(hdr[:]); err != nil {
			return fmt.Errorf("write extent header (off %d len %d): %w", e.Off, e.Len, err)
		}
		if _, err := io.Copy(w, io.NewSectionReader(f, e.Off, e.Len)); err != nil {
			return fmt.Errorf("write extent data (off %d len %d): %w", e.Off, e.Len, err)
		}
	}
	return nil
}

// ApplyExtents reads frames written by StreamExtents from r and
// pwrites each into f at its offset, extending f (with an implicit
// hole over any gap) as needed. f must already be open for writing.
// It returns the total data bytes written and the applied extents.
func ApplyExtents(r io.Reader, f *os.File) (int64, []Extent, error) {
	var (
		hdr     [16]byte
		total   int64
		extents []Extent
	)
	for {
		if _, err := io.ReadFull(r, hdr[:]); err != nil {
			if errors.Is(err, io.EOF) {
				break
			}
			return total, extents, fmt.Errorf("read extent header: %w", err)
		}
		off := int64(binary.LittleEndian.Uint64(hdr[0:8]))
		length := int64(binary.LittleEndian.Uint64(hdr[8:16]))

		n, err := io.CopyN(io.NewOffsetWriter(f, off), r, length)
		total += n
		if err != nil {
			return total, extents, fmt.Errorf("write extent data (off %d len %d): %w", off, length, err)
		}
		extents = append(extents, Extent{Off: off, Len: length})
	}
	return total, extents, nil
}

// Merge copies every allocated extent of the sparse file at srcPath
// into dstPath at the same offsets, then grows dstPath to at least
// srcPath's apparent size. This is the local equivalent of
// StreamExtents piped into ApplyExtents: applying a diff snapshot
// onto a base memory file.
func Merge(dstPath, srcPath string) (int64, int, error) {
	src, err := os.Open(srcPath)
	if err != nil {
		return 0, 0, fmt.Errorf("open %s: %w", srcPath, err)
	}
	defer src.Close()

	dst, err := os.OpenFile(dstPath, os.O_RDWR|os.O_CREATE, 0o644)
	if err != nil {
		return 0, 0, fmt.Errorf("open %s: %w", dstPath, err)
	}
	defer dst.Close()

	extents, err := WalkExtents(src)
	if err != nil {
		return 0, 0, err
	}
	var total int64
	for _, e := range extents {
		n, err := io.Copy(io.NewOffsetWriter(dst, e.Off), io.NewSectionReader(src, e.Off, e.Len))
		total += n
		if err != nil {
			return total, len(extents), fmt.Errorf("merge extent (off %d len %d) into %s: %w", e.Off, e.Len, dstPath, err)
		}
	}

	srcInfo, err := src.Stat()
	if err != nil {
		return total, len(extents), fmt.Errorf("stat %s: %w", srcPath, err)
	}
	dstInfo, err := dst.Stat()
	if err != nil {
		return total, len(extents), fmt.Errorf("stat %s: %w", dstPath, err)
	}
	if dstInfo.Size() < srcInfo.Size() {
		if err := dst.Truncate(srcInfo.Size()); err != nil {
			return total, len(extents), fmt.Errorf("truncate %s to %d: %w", dstPath, srcInfo.Size(), err)
		}
	}
	return total, len(extents), nil
}

// AllocatedBytes returns the number of bytes actually allocated to
// the file at path — its on-disk (non-hole) footprint — rather than
// its apparent size.
func AllocatedBytes(path string) (int64, error) {
	var st unix.Stat_t
	if err := unix.Stat(path, &st); err != nil {
		return 0, fmt.Errorf("stat %s: %w", path, err)
	}
	return st.Blocks * 512, nil
}
