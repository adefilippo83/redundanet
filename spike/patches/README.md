# quic-go: path probes are not sent while the current path is congestion limited

`quic-go-v0.64.1-probe-when-congestion-limited.patch` fixes it and adds an
integration test. Apply to a quic-go v0.64.1 checkout with `patch -p1`.

Verified on 2026-11-01 (Go 1.27.3, macOS arm64):

- The new test fails without the fix (3 of 3 runs, "the new path was never
  validated") and passes with it (5 of 5).
- With the fix, `go test ./integrationtests/self/ .` passes.
- In the transport spike, a 700 MiB transfer survived its path dying
  mid-transfer: 6 s for the application to notice, 112 ms to validate and
  switch to the new path.

Nothing has been posted upstream. The texts below are drafts for the
maintainer to review and file.

## Issue draft

**Title:** Client cannot migrate away from a dead path while it has data in flight

**Body:**

A client that loses its current path while sending (Wi-Fi gone, NAT mapping
expired, relay down) cannot move the connection to a new path. `Path.Probe`
never completes: the PATH_CHALLENGE for the new path is never sent.

Pending path probes are only sent from `sendPackets`, and `triggerSending`
only calls `sendPackets` when the send mode is `SendAny`. A path that died with
a full congestion window never returns to `SendAny`: no ACKs arrive, and the
connection alternates between PTO probes on the dead path and `SendNone` or
`SendAck`. The probe for the new path waits behind a congestion window that
will never open, until the idle timeout closes the connection.

It does not show when the application probes the new path within a few
milliseconds of the loss, which is why `TestConnectionMigration` passes. An
application that notices the loss through a keepalive timeout, as anything
running over real networks will, probes after the connection has entered
PTO backoff, and then the probe is never sent.

Reproduction: `TestConnectionMigrationAwayFromDeadPath` in the attached patch.
It starts a 48 MiB transfer with large flow-control windows (so the sender is
congestion limited), drops every packet on the first path through the test
proxy, waits 2 s, then calls `AddPath` and `Probe` on a second transport.
Without the fix `Probe` times out after 3 s.

Found in a userspace mesh that moves connections between a relay and a
direct path (quic-go v0.64.1).

## Pull request draft

**Title:** Send path probes regardless of the current path's send mode

**Body:**

Path probes were only sent from `sendPackets`, which `triggerSending` reaches
only in `SendAny` mode. A client whose current path died with data in flight
is congestion limited on that path indefinitely, so it could never validate a
new one.

This moves the probe into a helper, `maybeSendPathProbe`, and calls it at the
start of `triggerSending`, before the send-mode switch, as well as from
`sendPackets` as before. A probe is a single packet on another path, so the
congestion state of the old path should not hold it back. RFC 9000 section 9.4
gives each path its own congestion controller after a migration.

Adds `TestConnectionMigrationAwayFromDeadPath`, which fails without the change
and passes with it.

Fixes #(issue number).

## Two related observations, not filed

1. When a client switches from a busy fast path to a slower one while the
   fast path still works, the server drops the probes on the slow path as
   "potentially duplicate": by the time they arrive, the server's received
   packet history has moved past their packet numbers. RFC 9000 allows this,
   so it is a property of the design. It is worth a sentence in the
   connection migration docs: pause sending for about one RTT of the slower
   path before probing it.
2. The server appears to keep the connection ID it reserved for every path it
   switched to: in a trace after two migrations, the server's queue of the
   client's connection IDs was empty with two IDs reserved for paths, and a
   third migration was never answered ("skipping validation of new path ...
   since no connection ID is available"). Not yet isolated in a test.
