package shareproto

import (
	"context"
	"fmt"

	"github.com/adefilippo83/redundanet/agent/internal/identity"
	"github.com/adefilippo83/redundanet/agent/internal/object"
)

// Source names a share of an owner's object on a server, for a reader
// built on object.Decode.
type Source struct {
	Share        int
	Client       *Client
	Peer         identity.Key
	Owner        identity.Key
	StorageIndex object.StorageIndex
	Geometry     object.Geometry
}

// Sources fetches each share's hash list and returns decode sources that
// read one block at a time from its server. A share whose holder does not
// answer is left out; the decoder decides whether enough remain.
func Sources(ctx context.Context, shares []Source) ([]object.Source, error) {
	var out []object.Source
	var lastErr error
	for _, s := range shares {
		s := s
		_, hashes, err := s.Client.GetInfo(ctx, s.Peer, s.Owner, s.StorageIndex, uint8(s.Share))
		if err != nil {
			lastErr = err
			continue
		}
		out = append(out, object.Source{Share: s.Share, Hashes: hashes, Read: func(ctx context.Context, segment int) ([]byte, error) {
			var block []byte
			_, err := s.Client.GetShare(ctx, s.Peer, s.Owner, s.StorageIndex, uint8(s.Share), segment, segment+1, s.Geometry, func(_ int, b []byte) error { block = b; return nil })
			return block, err
		}})
	}
	if len(out) == 0 && lastErr != nil {
		return nil, fmt.Errorf("shareproto: no share could be read: %w", lastErr)
	}
	return out, nil
}
