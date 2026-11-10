package repair

import (
	"testing"

	"github.com/adefilippo83/redundanet/agent/internal/identity"
)

func TestReleasable(t *testing.T) {
	var a, b, liar identity.Key
	a[0], b[0], liar[0] = 1, 2, 9
	all3, all4 := []uint8{0, 1, 2}, []uint8{0, 1, 2, 3}
	for _, tc := range []struct {
		name    string
		n       uint8
		total   int
		servers int
		mine    []uint8
		others  map[identity.Key][]uint8
		ok      bool
	}{
		// What surplus looks like when everyone is honest.
		{"a duplicate of my share on another server, the rest spread", 0, 3, 3, []uint8{0, 1}, map[identity.Key][]uint8{a: {0}, b: {2}}, true},
		{"I hold two numbers, one is also elsewhere, 2-of-4 on two servers", 1, 4, 2, []uint8{0, 1}, map[identity.Key][]uint8{a: {1, 2, 3}}, false},
		{"the same, with the other holder within its part", 1, 4, 2, []uint8{0, 1, 2}, map[identity.Key][]uint8{a: {1, 3}}, true},
		{"after a repair onto a returning server", 2, 3, 3, []uint8{2}, map[identity.Key][]uint8{a: {0, 2}, b: {1}}, false},
		{"a third server holds the number: one number on its word is its part", 2, 3, 3, []uint8{2}, map[identity.Key][]uint8{a: {0}, b: {1}, liar: {2}}, true},
		// One server's word for everything is worth its part and no more.
		{"one server says it holds every number, 2-of-3 on three servers", 0, 3, 3, []uint8{0}, map[identity.Key][]uint8{liar: all3}, false},
		{"the same beside an honest holder of one", 0, 3, 3, []uint8{0}, map[identity.Key][]uint8{liar: all3, a: {1}}, false},
		{"the same when I hold two", 0, 3, 3, []uint8{0, 1}, map[identity.Key][]uint8{liar: all3}, false},
		{"one server says it holds every number, 2-of-4 on three servers", 0, 4, 3, []uint8{0, 1}, map[identity.Key][]uint8{liar: all4, a: {2}}, true},
		{"and then my last one is not released on the same word", 1, 4, 3, []uint8{1}, map[identity.Key][]uint8{liar: all4, a: {2}}, false},
		{"a single server network has nobody else", 0, 3, 1, all3, map[identity.Key][]uint8{}, false},
		// The number itself must be on another holder.
		{"nobody else holds the number", 0, 3, 3, []uint8{0}, map[identity.Key][]uint8{a: {1}, b: {2}}, false},
		{"a number outside the object", 3, 3, 3, all3, map[identity.Key][]uint8{a: all3}, false},
		// A holder whose part is taken by other numbers gives way when
		// another can hold one of them.
		{"the matching moves a number to make room", 0, 3, 3, []uint8{0}, map[identity.Key][]uint8{a: {0, 1}, b: {1}, liar: {2}}, true},
	} {
		err := releasable(tc.n, tc.total, tc.servers, tc.mine, tc.others)
		if (err == nil) != tc.ok {
			t.Errorf("%s: releasable = %v, want ok %v", tc.name, err, tc.ok)
		}
	}
}
