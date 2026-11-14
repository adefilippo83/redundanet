package shareproto

import (
	"context"
	"log/slog"

	"github.com/adefilippo83/redundanet/agent/internal/identity"
	"github.com/adefilippo83/redundanet/agent/internal/wire"
)

// Remote is what a device answers when another device of the same member
// looks at it: its status, and the few actions a member may start from
// afar. The protocol has a message for nothing else, so what cannot be
// asked cannot be done: no restore, no setting, no pairing, no phrase.
type Remote struct {
	// Member is this device's member. Only a device certified for the
	// same member is answered; the mesh has verified the certificate.
	Member identity.Key
	// View is the device's status, encoded as the agents agreed.
	View func() ([]byte, error)
	// Act starts the named action. An unknown name is an error.
	Act func(action string) error
}

// maxView bounds a view's body under the limit of one message.
const maxView = maxMessage - 4096

// Serve is the stream handler of a device: the storage protocol when it
// has a store, a refusal of it when it has none, and the member's remote
// view either way when remote is not nil.
func Serve(storage *Server, remote *Remote, log *slog.Logger) func(identity.Peer, string, wire.Stream) {
	if log == nil {
		log = slog.Default()
	}
	log = log.With("component", "shareproto")
	return func(peer identity.Peer, _ string, st wire.Stream) {
		defer st.Close()
		defer st.CancelRead(0)
		var req Request
		if err := readMsg(st, &req); err != nil {
			st.CancelRead(1)
			return
		}
		switch {
		case req.Type == TypeView || req.Type == TypeAct:
			if err := remote.serve(peer, st, &req, log); err != nil {
				st.CancelRead(1)
			}
		case storage != nil:
			storage.serve(peer, st, &req)
		default:
			_ = writeMsg(st, Response{Status: StatusRefused, Message: "this device is not a storage node"})
		}
	}
}

func (r *Remote) serve(peer identity.Peer, st wire.Stream, req *Request, log *slog.Logger) error {
	if r == nil {
		return writeMsg(st, Response{Status: StatusRefused, Message: "this device does not answer its member's other devices"})
	}
	if r.Member.IsZero() || peer.Member != r.Member {
		log.Info("remote view refused: the device is another member's", "peer", peer.Device.Short())
		return writeMsg(st, Response{Status: StatusRefused, Message: "this device answers only its own member's devices"})
	}
	if req.Type == TypeView {
		body, err := r.View()
		if err != nil {
			return writeMsg(st, Response{Status: StatusFailed, Message: err.Error()})
		}
		if len(body) > maxView {
			return writeMsg(st, Response{Status: StatusFailed, Message: "the status is too large to send"})
		}
		return writeMsg(st, Response{Status: StatusOK, Body: body})
	}
	if err := r.Act(req.Action); err != nil {
		return writeMsg(st, Response{Status: StatusBad, Message: err.Error()})
	}
	log.Info("started from another device of the member", "action", req.Action, "peer", peer.Device.Short())
	return writeMsg(st, Response{Status: StatusOK})
}

// View asks another device of the member for its status.
func (c *Client) View(ctx context.Context, peer identity.Key) ([]byte, error) {
	st, resp, err := c.exchange(ctx, peer, &Request{Type: TypeView})
	if err != nil {
		return nil, err
	}
	done(st)
	return resp.Body, nil
}

// Act asks another device of the member to start the named action.
func (c *Client) Act(ctx context.Context, peer identity.Key, action string) error {
	st, _, err := c.exchange(ctx, peer, &Request{Type: TypeAct, Action: action})
	if err != nil {
		return err
	}
	done(st)
	return nil
}
