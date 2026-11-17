package store

import (
	"errors"
	"sync"
	"testing"
)

// Two devices of one member that back up the same files send the same
// share to a server at the same moment. Whatever the order they begin in,
// the share ends up complete in the index with its file on the disk: the
// second to begin never removes what the first is writing.
func TestTheSameShareSentTwiceAtOnceKeepsItsFile(t *testing.T) {
	s, _ := newStore(t)
	o := newOwner(t, 1)
	const objects = 150
	for i := 0; i < objects; i++ {
		ob := encode(t, o, data(3000, byte(i+1)))
		start := make(chan struct{})
		var wg sync.WaitGroup
		for d := 0; d < 2; d++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				<-start
				p, err := s.BeginPut(ob.sd, 0)
				if errors.Is(err, ErrAlreadyComplete) {
					return
				}
				if err != nil {
					t.Errorf("object %d: begin: %v", i, err)
					return
				}
				for seg := p.Resume(); seg < p.Segments(); seg++ {
					if err := p.WriteBlock(seg, ob.blocks[0][seg]); err != nil {
						p.Abort()
						return
					}
				}
				_ = p.Complete()
			}()
		}
		close(start)
		wg.Wait()
		si := ob.sd.Descriptor.StorageIndex
		pres, err := s.Has(o.member, si)
		if err != nil || pres == nil || len(pres.Complete) != 1 {
			t.Fatalf("object %d: the index after both puts: %+v %v", i, pres, err)
		}
		sh, err := s.OpenShare(o.member, si, 0)
		if err != nil {
			t.Fatalf("object %d: complete in the index and not on the disk: %v", i, err)
		}
		verr := sh.Verify()
		sh.Close()
		if verr != nil {
			t.Fatalf("object %d: the share does not verify: %v", i, verr)
		}
	}
}
