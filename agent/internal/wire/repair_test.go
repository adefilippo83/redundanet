package wire

import (
	"strings"
	"testing"
	"time"

	"github.com/adefilippo83/redundanet/agent/internal/identity"
)

func TestRepairTaskSignature(t *testing.T) {
	hub, _ := identity.NewDeviceKey()
	var worker, owner, server identity.Key
	worker[0], owner[0], server[0] = 1, 2, 3
	now := time.Date(2026, 11, 3, 12, 0, 0, 0, time.UTC)
	task := &RepairTask{Network: "demo", ID: 7, Owner: owner, Worker: worker, Shares: []RepairShare{{Number: 2, Server: server}}, Issued: now, Expires: now.Add(time.Hour)}
	task.StorageIndex[0], task.DescriptorHash[0] = 9, 8
	if err := task.Sign(hub); err != nil {
		t.Fatal(err)
	}
	if task.Hub != hub.ID() || len(task.Signature) == 0 {
		t.Fatalf("signed: %+v", task)
	}
	// It survives the wire.
	b, err := identity.Marshal(task)
	if err != nil {
		t.Fatal(err)
	}
	var back RepairTask
	if err := identity.UnmarshalLenient(b, &back); err != nil {
		t.Fatal(err)
	}
	if err := back.Verify("demo", now.Add(30*time.Minute)); err != nil {
		t.Fatalf("verify: %v", err)
	}
	if !back.Places(server, 2) || back.Places(server, 1) || back.Places(worker, 2) {
		t.Fatal("Places answers the wrong servers or numbers")
	}
	// Every change breaks it; an expired or foreign task is refused.
	cases := map[string]func(x *RepairTask){
		"another network":  func(x *RepairTask) { x.Network = "other" },
		"another worker":   func(x *RepairTask) { x.Worker[0] = 5 },
		"another server":   func(x *RepairTask) { x.Shares[0].Server[0] = 5 },
		"another number":   func(x *RepairTask) { x.Shares[0].Number = 3 },
		"another object":   func(x *RepairTask) { x.StorageIndex[1] = 1 },
		"another hub":      func(x *RepairTask) { k, _ := identity.NewDeviceKey(); x.Hub = k.ID() },
		"a longer expiry":  func(x *RepairTask) { x.Expires = x.Expires.Add(time.Hour) },
		"no shares":        func(x *RepairTask) { x.Shares = nil },
		"a broken signing": func(x *RepairTask) { x.Signature[3] ^= 1 },
	}
	for name, mutate := range cases {
		x := back
		x.Shares = append([]RepairShare(nil), back.Shares...)
		x.Signature = append([]byte(nil), back.Signature...)
		mutate(&x)
		if err := x.Verify("demo", now.Add(30*time.Minute)); err == nil {
			t.Fatalf("%s verified", name)
		}
	}
	if err := back.Verify("demo", now.Add(2*time.Hour)); err == nil || !strings.Contains(err.Error(), "expired") {
		t.Fatalf("expired task: %v", err)
	}
}
