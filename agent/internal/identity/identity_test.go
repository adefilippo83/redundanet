package identity

import (
	"encoding/hex"
	"regexp"
	"strings"
	"testing"
	"time"
)

func testSeed() Seed {
	var s Seed
	for i := range s {
		s[i] = byte(i)
	}
	return s
}

// The derivation is pinned: a change to a label or an algorithm changes every
// member's keys, and this test is what says so.
func TestKnownAnswers(t *testing.T) {
	s := testSeed()
	if got := strings.Join(s.Phrase(), " "); got != "abandon amount liar amount expire adjust cage candy arch gather drum buyer" {
		t.Fatalf("phrase: %s", got)
	}
	if got := s.MemberKey().String(); got != "q2ydvuuplwr7cajell7t7uhbv672f4y6tdiwzptywwiu2djjy6da" {
		t.Fatalf("member key: %s", got)
	}
	sec := s.Secret()
	if got := hex.EncodeToString(sec[:]); got != "eb90a152227c12ec7f4ff43ebdfec7d9ff5603c4928686c0f27a3f41d0df77bb" {
		t.Fatalf("secret: %s", got)
	}
	if got := hex.EncodeToString(sec.Subkey("convergence", 32)); got != "d77c277886765f3c98b89ac5dc76922c92d96a79168007fbfb3abbd6259ee445" {
		t.Fatalf("convergence subkey: %s", got)
	}
	if got := DisplayCode(s.MemberKey()); got != "J3TL FJWU DX5E GDNI HYH3 AG3H K25Q OT4Y 4IAX LOJV" {
		t.Fatalf("display code: %s", got)
	}
}

func TestPhraseRoundTrip(t *testing.T) {
	s, err := NewSeed()
	if err != nil {
		t.Fatal(err)
	}
	words := s.Phrase()
	if len(words) != 12 {
		t.Fatalf("%d words", len(words))
	}
	back, err := ParsePhrase(words)
	if err != nil {
		t.Fatal(err)
	}
	if back != s {
		t.Fatal("round trip changed the seed")
	}
	// Case, spacing and a single string are all accepted.
	upper := []string{strings.ToUpper(strings.Join(words[:6], "  ")), strings.Join(words[6:], " ")}
	if back, err = ParsePhrase(upper); err != nil || back != s {
		t.Fatalf("tolerant parse: %v", err)
	}
}

func TestPhraseRejectsErrors(t *testing.T) {
	words := testSeed().Phrase()
	bad := append([]string(nil), words...)
	bad[3] = "zoo"
	if _, err := ParsePhrase(bad); err == nil {
		t.Fatal("a wrong word passed the checksum")
	}
	bad = append([]string(nil), words...)
	bad[0], bad[1] = bad[1], bad[0]
	if _, err := ParsePhrase(bad); err == nil {
		t.Fatal("swapped words passed the checksum")
	}
	if _, err := ParsePhrase(words[:11]); err == nil {
		t.Fatal("eleven words accepted")
	}
	bad = append([]string(nil), words...)
	bad[5] = "notaword"
	if _, err := ParsePhrase(bad); err == nil {
		t.Fatal("a word outside the list accepted")
	}
}

func TestMemberDerivationIsDeterministicAndSeparated(t *testing.T) {
	s := testSeed()
	if s.MemberKey() != s.MemberKey() {
		t.Fatal("member key differs between derivations")
	}
	sec := s.Secret()
	if string(sec.Subkey("convergence", 32)) == string(sec.Subkey("root", 32)) {
		t.Fatal("two purposes share a subkey")
	}
	var other Seed
	other[0] = 1
	if other.MemberKey() == s.MemberKey() {
		t.Fatal("different seeds gave the same key")
	}
}

func TestDisplayCodeShape(t *testing.T) {
	code := DisplayCode(testSeed().MemberKey())
	if !regexp.MustCompile(`^([A-Z2-7]{4} ){9}[A-Z2-7]{4}$`).MatchString(code) {
		t.Fatalf("shape: %q", code)
	}
	if strings.ContainsAny(code, "018") {
		t.Fatalf("ambiguous characters in %q", code)
	}
}

func TestKeyStringRoundTrip(t *testing.T) {
	k := testSeed().MemberKey()
	if len(k.String()) != 52 {
		t.Fatalf("length %d", len(k.String()))
	}
	back, err := ParseKey(strings.ToUpper(" " + k.String() + "\n"))
	if err != nil || back != k {
		t.Fatalf("round trip: %v", err)
	}
	if _, err := ParseKey(k.String()[:51]); err == nil {
		t.Fatal("short key accepted")
	}
}

func TestDeviceSignatures(t *testing.T) {
	d, err := NewDeviceKey()
	if err != nil {
		t.Fatal(err)
	}
	sig := d.Sign("storage-request", []byte("body"))
	if err := VerifyDevice(d.ID(), "storage-request", []byte("body"), sig); err != nil {
		t.Fatal(err)
	}
	if err := VerifyDevice(d.ID(), "delete-token", []byte("body"), sig); err == nil {
		t.Fatal("a signature verified under another domain")
	}
	if err := VerifyDevice(d.ID(), "storage-request", []byte("bodx"), sig); err == nil {
		t.Fatal("a signature verified over another body")
	}
	back, err := DeviceKeyFromSeed(d.Seed())
	if err != nil || back.ID() != d.ID() {
		t.Fatalf("seed round trip: %v", err)
	}
	if _, err := DeviceKeyFromSeed(d.Seed()[:31]); err == nil {
		t.Fatal("short seed accepted")
	}
}

func issueTestCert(t *testing.T, seed Seed, roles ...string) (*DeviceKey, *SignedCertificate) {
	t.Helper()
	d, err := NewDeviceKey()
	if err != nil {
		t.Fatal(err)
	}
	sc, err := Issue(seed, Certificate{Network: "testnet", Device: d.ID(), Roles: roles, IssuedAt: time.Now(), Serial: 1})
	if err != nil {
		t.Fatal(err)
	}
	return d, sc
}

func TestCertificateIssueVerifyEncode(t *testing.T) {
	seed := testSeed()
	d, sc := issueTestCert(t, seed, RoleClient, RoleStorage)
	if err := sc.Verify(); err != nil {
		t.Fatal(err)
	}
	if sc.Certificate.Member != seed.MemberKey() || sc.Certificate.Device != d.ID() {
		t.Fatal("certificate names the wrong keys")
	}
	if !sc.HasRole(RoleStorage) || sc.HasRole(RoleHub) {
		t.Fatal("roles")
	}
	b, err := sc.Encode()
	if err != nil {
		t.Fatal(err)
	}
	back, err := DecodeCertificate(b)
	if err != nil {
		t.Fatal(err)
	}
	if back.Certificate.Network != sc.Certificate.Network || back.Certificate.Device != sc.Certificate.Device ||
		back.Certificate.Member != sc.Certificate.Member || back.Certificate.Serial != sc.Certificate.Serial ||
		!back.Certificate.IssuedAt.Equal(sc.Certificate.IssuedAt) || strings.Join(back.Certificate.Roles, ",") != strings.Join(sc.Certificate.Roles, ",") {
		t.Fatal("decode changed the certificate")
	}
	// Deterministic: the same certificate encodes to the same bytes twice.
	b2, _ := sc.Encode()
	if string(b) != string(b2) {
		t.Fatal("encoding is not deterministic")
	}
}

func TestCertificateRejectsTampering(t *testing.T) {
	seed := testSeed()
	_, sc := issueTestCert(t, seed, RoleClient)
	tampered := *sc
	tampered.Certificate.Roles = []string{RoleClient, RoleStorage}
	if err := tampered.Verify(); err == nil {
		t.Fatal("a changed role verified")
	}
	tampered = *sc
	tampered.Certificate.Network = "othernet"
	if err := tampered.Verify(); err == nil {
		t.Fatal("a changed network verified")
	}
	var other Seed
	other[0] = 7
	tampered = *sc
	tampered.Certificate.Member = other.MemberKey()
	if err := tampered.Verify(); err == nil {
		t.Fatal("a swapped member key verified")
	}
	tampered = *sc
	tampered.Signature = append([]byte(nil), sc.Signature...)
	tampered.Signature[0] ^= 1
	if err := tampered.Verify(); err == nil {
		t.Fatal("a flipped signature bit verified")
	}
}

func TestCertificateShape(t *testing.T) {
	seed := testSeed()
	d, _ := NewDeviceKey()
	cases := map[string]Certificate{
		"no network": {Device: d.ID(), Roles: []string{RoleClient}, IssuedAt: time.Now()},
		"no device":  {Network: "n", Roles: []string{RoleClient}, IssuedAt: time.Now()},
		"no roles":   {Network: "n", Device: d.ID(), IssuedAt: time.Now()},
		"bad role":   {Network: "n", Device: d.ID(), Roles: []string{"admin"}, IssuedAt: time.Now()},
		"no time":    {Network: "n", Device: d.ID(), Roles: []string{RoleClient}},
	}
	for name, c := range cases {
		if _, err := Issue(seed, c); err == nil {
			t.Errorf("%s: issued", name)
		}
	}
	// Unknown fields in the encoding are refused, so a record from a newer
	// version is never half-read.
	_, sc := issueTestCert(t, seed, RoleClient)
	b, _ := sc.Encode()
	var loose map[string]any
	if err := Unmarshal(b, &loose); err != nil {
		t.Fatal(err)
	}
	loose["extra"] = 1
	b2, _ := Marshal(loose)
	if _, err := DecodeCertificate(b2); err == nil {
		t.Fatal("an unknown field was accepted")
	}
}

func TestRevocation(t *testing.T) {
	seed := testSeed()
	d, sc := issueTestCert(t, seed, RoleClient)
	rv, err := Revoke(seed, Revocation{Network: "testnet", Device: d.ID(), Serial: 1})
	if err != nil {
		t.Fatal(err)
	}
	if err := rv.Verify(); err != nil {
		t.Fatal(err)
	}
	if !rv.Covers(sc) {
		t.Fatal("revocation does not cover its certificate")
	}
	other := *rv
	other.Revocation.Serial = 2
	if other.Covers(sc) {
		t.Fatal("a revocation for another serial covers the certificate")
	}
	b, _ := rv.Encode()
	back, err := DecodeRevocation(b)
	if err != nil || !back.Covers(sc) {
		t.Fatalf("decode: %v", err)
	}
	b[len(b)-1] ^= 1
	if _, err := DecodeRevocation(b); err == nil {
		t.Fatal("a tampered revocation decoded")
	}
	if _, err := Revoke(seed, Revocation{Network: "testnet"}); err == nil {
		t.Fatal("a revocation without a device was signed")
	}
}
