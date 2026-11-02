package object

import (
	"encoding/binary"
	"errors"
	"fmt"
	"io"
)

// A share as a byte stream, on disk and on the wire: a header with the
// signed descriptor, the blocks in segment order, then the block hashes.
// The hashes come last so a writer streaming blocks as it encodes them
// needs nothing it does not yet have.
//
//	magic "RN3S" | version u8 | share number u8 | descriptor length u32 BE | signed descriptor CBOR
//	block[0] ... block[segments-1]
//	hash[0] ... hash[segments-1]

var shareMagic = [4]byte{'R', 'N', '3', 'S'}

// MaxDescriptor bounds a header: a descriptor with 255 share roots and a
// certificate is a few kilobytes.
const MaxDescriptor = 64 << 10

// ShareHeader is what a share stream starts with.
type ShareHeader struct {
	Version    uint8
	Number     uint8
	Descriptor *SignedDescriptor
	Len        int // bytes the header occupies in the stream
}

// WriteShareHeader writes the header for share number of the object.
func WriteShareHeader(w io.Writer, sd *SignedDescriptor, number uint8) (int, error) {
	if int(number) >= int(sd.Descriptor.N) {
		return 0, fmt.Errorf("object: share number %d of n=%d", number, sd.Descriptor.N)
	}
	body, err := sd.Encode()
	if err != nil {
		return 0, err
	}
	if len(body) > MaxDescriptor {
		return 0, errors.New("object: descriptor too large")
	}
	hdr := make([]byte, 0, 10+len(body))
	hdr = append(hdr, shareMagic[:]...)
	hdr = append(hdr, FormatVersion, number)
	hdr = binary.BigEndian.AppendUint32(hdr, uint32(len(body)))
	hdr = append(hdr, body...)
	n, err := w.Write(hdr)
	return n, err
}

// ReadShareHeader reads a header; the descriptor is parsed, not verified.
func ReadShareHeader(r io.Reader) (*ShareHeader, error) {
	var fixed [10]byte
	if _, err := io.ReadFull(r, fixed[:]); err != nil {
		return nil, fmt.Errorf("object: share header: %w", err)
	}
	if [4]byte(fixed[:4]) != shareMagic {
		return nil, errors.New("object: not a share")
	}
	if fixed[4] != FormatVersion {
		return nil, fmt.Errorf("object: share version %d is not %d", fixed[4], FormatVersion)
	}
	n := binary.BigEndian.Uint32(fixed[6:10])
	if n == 0 || n > MaxDescriptor {
		return nil, errors.New("object: share header length out of range")
	}
	body := make([]byte, n)
	if _, err := io.ReadFull(r, body); err != nil {
		return nil, fmt.Errorf("object: share header: %w", err)
	}
	sd, err := DecodeSignedDescriptor(body)
	if err != nil {
		return nil, err
	}
	if int(fixed[5]) >= int(sd.Descriptor.N) {
		return nil, fmt.Errorf("object: share number %d of n=%d", fixed[5], sd.Descriptor.N)
	}
	return &ShareHeader{Version: fixed[4], Number: fixed[5], Descriptor: sd, Len: 10 + int(n)}, nil
}

// WriteShareTrailer writes the block hashes.
func WriteShareTrailer(w io.Writer, hashes []Hash) error {
	buf := make([]byte, 0, len(hashes)*len(Hash{}))
	for _, h := range hashes {
		buf = append(buf, h[:]...)
	}
	_, err := w.Write(buf)
	return err
}

// ReadShareTrailer reads the block hashes of an object with that many segments.
func ReadShareTrailer(r io.Reader, segments int) ([]Hash, error) {
	hashes := make([]Hash, segments)
	for i := range hashes {
		if _, err := io.ReadFull(r, hashes[i][:]); err != nil {
			return nil, fmt.Errorf("object: share trailer: %w", err)
		}
	}
	return hashes, nil
}

// BlockOffset is where block s of a share starts, after a header of hdrLen bytes.
func BlockOffset(g Geometry, hdrLen int, s int) int64 {
	off := int64(hdrLen)
	for i := 0; i < s; i++ {
		off += int64(g.BlockSize(i))
	}
	return off
}

// ShareSize is the whole stream: header, blocks and trailer.
func ShareSize(g Geometry, hdrLen int) int64 {
	return int64(hdrLen) + g.ShareBlocksSize() + int64(g.Segments())*int64(len(Hash{}))
}
