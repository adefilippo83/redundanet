// Package identity is the member and device identity of RFC 0001: a member
// is a 128-bit seed shown once as a twelve-word phrase, everything else is
// derived from it or certified by it.
//
//   - The member signing key is derived from the seed when one of the three
//     member-level acts needs it (sign a join request, certify a device,
//     revoke one) and is never stored.
//   - The member secret is derived from the seed too, lives on every device
//     of the member, and is what the storage engine derives its convergence
//     secret and root key from.
//   - A device key is random per device and signs everything the device does
//     every day: its transport identity, storage requests, its root record.
//   - A device certificate binds a device key to a member key, signed by the
//     member key, so servers and peers trust devices they have never seen.
//
// Encodings: public keys and device IDs are 32 bytes shown as 52 lowercase
// base32 characters; signed records are deterministic CBOR with the
// signature over a domain-separated encoding of the unsigned record.
package identity
