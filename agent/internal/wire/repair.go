package wire

import (
	"errors"
	"fmt"
	"time"

	"github.com/adefilippo83/redundanet/agent/internal/identity"
)

// Repair without the owner, RFC 0001's "Health" section: the scheduling
// hub turns an object that needs repair into a task signed by its device
// key, hands it to a storage node that volunteers as a worker, and the
// servers the task names accept the regenerated shares from that worker
// because the task says so. The owner is never involved.

// RepairTask is one object's repair order.
type RepairTask struct {
	Network        string         `cbor:"network" json:"network"`
	Hub            identity.Key   `cbor:"hub" json:"hub"` // the scheduling hub's device key, which the manifest lists
	ID             uint64         `cbor:"id" json:"id"`   // unique per hub
	StorageIndex   [16]byte       `cbor:"si" json:"si"`
	Owner          identity.Key   `cbor:"owner" json:"owner"`
	DescriptorHash [32]byte       `cbor:"dhash" json:"dhash"`
	Worker         identity.Key   `cbor:"worker" json:"worker"`
	Holders        []identity.Key `cbor:"holders" json:"holders"` // servers believed to hold complete shares: where to fetch k from, a hint
	Shares         []RepairShare  `cbor:"shares" json:"shares"`   // what to regenerate and where each goes
	Issued         time.Time      `cbor:"issued" json:"issued"`
	Expires        time.Time      `cbor:"expires" json:"expires"`
	Signature      []byte         `cbor:"signature,omitempty" json:"signature,omitempty"`
}

// RepairShare is one share number to store on one server.
type RepairShare struct {
	Number uint8        `cbor:"n" json:"n"`
	Server identity.Key `cbor:"server" json:"server"`
}

const domainRepairTask = "repair-task"

// Sign sets the hub key and the signature; the hub's device key signs.
func (t *RepairTask) Sign(dev *identity.DeviceKey) error {
	t.Hub = dev.ID()
	t.Signature = nil
	body, err := identity.Marshal(t)
	if err != nil {
		return err
	}
	t.Signature = dev.Sign(domainRepairTask, body)
	return nil
}

// Verify checks the task's shape, that it is for this network and still
// current, and the hub's signature over everything but the signature.
// Whether the hub is one the manifest lists is the caller's question.
func (t *RepairTask) Verify(network string, now time.Time) error {
	if t.Network != network {
		return fmt.Errorf("repair task for network %q", t.Network)
	}
	if t.Hub.IsZero() || t.Worker.IsZero() || t.Owner.IsZero() {
		return errors.New("repair task without a hub, a worker or an owner")
	}
	if len(t.Shares) == 0 {
		return errors.New("repair task places nothing")
	}
	if t.Expires.IsZero() || !now.Before(t.Expires) {
		return errors.New("repair task has expired")
	}
	cp := *t
	cp.Signature = nil
	body, err := identity.Marshal(&cp)
	if err != nil {
		return err
	}
	if err := identity.VerifyDevice(t.Hub, domainRepairTask, body, t.Signature); err != nil {
		return fmt.Errorf("repair task: %w", err)
	}
	return nil
}

// Places reports whether the task stores share number n on the server.
func (t *RepairTask) Places(server identity.Key, n uint8) bool {
	for _, s := range t.Shares {
		if s.Server == server && s.Number == n {
			return true
		}
	}
	return false
}

// TaskReport is the worker's word on a task, back to the hub that sent it.
type TaskReport struct {
	ID     uint64  `cbor:"id" json:"id"`
	Done   []uint8 `cbor:"done" json:"done"`                         // share numbers stored
	Failed string  `cbor:"failed,omitempty" json:"failed,omitempty"` // why the rest was not
}

// ReleaseHint tells a holder that the object has more complete shares
// than n and that its copy of a share number is the one to give up. The
// holder asks the other holders itself before it removes anything: a
// lying hub can waste a release, never a needed share.
type ReleaseHint struct {
	StorageIndex   [16]byte       `cbor:"si" json:"si"`
	Number         uint8          `cbor:"n" json:"n"`
	DescriptorHash [32]byte       `cbor:"dhash" json:"dhash"`
	Holders        []identity.Key `cbor:"holders" json:"holders"` // the other servers that hold complete shares
}
