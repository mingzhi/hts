// Copyright 2009 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

// Changes ©2012 Dan Kortschak <dan.kortschak@adelaide.edu.au>

// Package egzip implements reading and writing of gzip format compressed files,
// as specified in RFC 1952, changing some behaviours implemented by the Go core
// library while maintaining conformance with the specification.
package egzip

import (
	"code.google.com/p/biogo.bam/bgzf/bufio"
	"compress/flate"
	"compress/gzip"
	"errors"
	"hash"
	"hash/crc32"
	"io"
	"time"
)

const (
	gzipID1     = 0x1f
	gzipID2     = 0x8b
	gzipDeflate = 8
	flagText    = 1 << 0
	flagHdrCrc  = 1 << 1
	flagExtra   = 1 << 2
	flagName    = 1 << 3
	flagComment = 1 << 4
)

var (
	ErrNotASeeker = errors.New("egzip: not a seeker")
	ErrNewBlock   = errors.New("egzip: new block")
)

func makeReader(r io.Reader) flate.Reader {
	if rr, ok := r.(flate.Reader); ok {
		return rr
	}
	return bufio.NewReader(r)
}

// A Reader is an io.Reader that can be read to retrieve
// uncompressed data from a gzip-format compressed file.
//
// In general, a gzip file can be a concatenation of gzip files,
// each with its own header.  Reads from the Reader
// return the concatenation of the uncompressed data of each.
// Only the first header is recorded in the Reader fields.
//
// Gzip files store a length and checksum of the uncompressed data.
// The Reader will return a ErrChecksum when Read
// reaches the end of the uncompressed data if it does not
// have the expected length or checksum.  Clients should treat data
// returned by Read as tentative until they receive the io.EOF
// marking the end of the data.
type Reader struct {
	BlockLimited bool // Stop reading at the end of a member and return ErrNewBlock.
	*gzip.Header
	r            flate.Reader
	s            io.Seeker
	decompressor io.ReadCloser
	digest       hash.Hash32
	size         uint32
	flg          byte
	buf          [512]byte
	err          error
}

// NewReader creates a new Reader reading the given reader.
// The implementation buffers input and may read more data than necessary from r.
// It is the caller's responsibility to call Close on the Reader when done.
func NewReader(r io.Reader, h *gzip.Header) (*Reader, error) {
	z := new(Reader)
	z.Header = h
	z.r = makeReader(r)
	z.digest = crc32.NewIEEE()
	z.s, _ = r.(io.Seeker)
	if err := z.readHeader(); err != nil {
		return nil, err
	}
	return z, nil
}

// GZIP (RFC 1952) is little-endian, unlike ZLIB (RFC 1950).
func get4(p []byte) uint32 {
	return uint32(p[0]) | uint32(p[1])<<8 | uint32(p[2])<<16 | uint32(p[3])<<24
}

func (z *Reader) readString() (string, error) {
	var err error
	needconv := false
	for i := 0; ; i++ {
		if i >= len(z.buf) {
			return "", gzip.ErrHeader
		}
		z.buf[i], err = z.r.ReadByte()
		if err != nil {
			return "", err
		}
		if z.buf[i] > 0x7f {
			needconv = true
		}
		if z.buf[i] == 0 {
			// GZIP (RFC 1952) specifies that strings are NUL-terminated ISO 8859-1 (Latin-1).
			if needconv {
				s := make([]rune, 0, i)
				for _, v := range z.buf[0:i] {
					s = append(s, rune(v))
				}
				return string(s), nil
			}
			return string(z.buf[0:i]), nil
		}
	}
	panic("not reached")
}

func (z *Reader) read2() (uint32, error) {
	_, err := io.ReadFull(z.r, z.buf[0:2])
	if err != nil {
		return 0, err
	}
	return uint32(z.buf[0]) | uint32(z.buf[1])<<8, nil
}

func (z *Reader) readHeader() error {
	_, err := io.ReadFull(z.r, z.buf[0:10])
	if err != nil {
		return err
	}
	if z.buf[0] != gzipID1 || z.buf[1] != gzipID2 || z.buf[2] != gzipDeflate {
		return gzip.ErrHeader
	}
	z.flg = z.buf[3]
	z.ModTime = time.Unix(int64(get4(z.buf[4:8])), 0)
	// z.buf[8] is xfl, ignored
	z.OS = z.buf[9]
	z.digest.Reset()
	z.digest.Write(z.buf[0:10])

	if z.flg&flagExtra != 0 {
		n, err := z.read2()
		if err != nil {
			return err
		}
		data := make([]byte, n)
		if _, err = io.ReadFull(z.r, data); err != nil {
			return err
		}
		z.Extra = data
	}

	var s string
	if z.flg&flagName != 0 {
		if s, err = z.readString(); err != nil {
			return err
		}
		z.Name = s
	}

	if z.flg&flagComment != 0 {
		if s, err = z.readString(); err != nil {
			return err
		}
		z.Comment = s
	}

	if z.flg&flagHdrCrc != 0 {
		n, err := z.read2()
		if err != nil {
			return err
		}
		sum := z.digest.Sum32() & 0xFFFF
		if n != sum {
			return gzip.ErrHeader
		}
	}

	z.digest.Reset()
	z.decompressor = flate.NewReader(z.r)
	return nil
}

func (z *Reader) Seek(offset int64, whence int) error {
	if z.s == nil {
		z.err = ErrNotASeeker
		return z.err
	}

	_, z.err = z.s.Seek(offset, whence)
	if z.err != nil {
		return z.err
	}
	z.r.(*bufio.Reader).Reset()

	err := z.readHeader()
	if err != nil {
		return err
	}

	z.err = nil
	z.digest.Reset()
	z.size = 0
	return nil
}

func (z *Reader) Read(p []byte) (n int, err error) {
	if z.err != nil {
		return 0, z.err
	}
	if len(p) == 0 {
		return 0, nil
	}

	n, err = z.decompressor.Read(p)
	z.digest.Write(p[0:n])
	z.size += uint32(n)
	if n != 0 || err != io.EOF {
		z.err = err
		return
	}

	// Finished file; check checksum + size.
	if _, err := io.ReadFull(z.r, z.buf[0:8]); err != nil {
		z.err = err
		return 0, err
	}
	crc32, isize := get4(z.buf[0:4]), get4(z.buf[4:8])
	sum := z.digest.Sum32()
	if sum != crc32 || isize != z.size {
		z.err = gzip.ErrChecksum
		return 0, z.err
	}

	// File is ok; is there another?
	if err = z.readHeader(); err != nil {
		z.err = err
		return
	}

	// Yes. Reset and read from it if not block limited.
	z.digest.Reset()
	z.size = 0
	if z.BlockLimited {
		err = ErrNewBlock
		return
	}
	return z.Read(p)
}

// Close closes the Reader. It does not close the underlying io.Reader.
func (z *Reader) Close() error { return z.decompressor.Close() }
