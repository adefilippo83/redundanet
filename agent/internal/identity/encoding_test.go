package identity

import "testing"

func TestUnmarshalLenientSkipsUnknownFields(t *testing.T) {
	type v1 struct {
		A int `cbor:"a"`
	}
	type v2 struct {
		A int `cbor:"a"`
		B int `cbor:"b"`
	}
	b, err := Marshal(v2{A: 1, B: 2})
	if err != nil {
		t.Fatal(err)
	}
	var old v1
	if err := Unmarshal(b, &old); err == nil {
		t.Fatal("strict decoding accepted a field the receiver does not know")
	}
	if err := UnmarshalLenient(b, &old); err != nil || old.A != 1 {
		t.Fatalf("lenient decoding: %v, %+v", err, old)
	}
	// Still strict about the properties that matter for every message.
	dup := []byte{0xa2, 0x61, 'a', 0x01, 0x61, 'a', 0x02}
	if err := UnmarshalLenient(dup, &old); err == nil {
		t.Fatal("lenient decoding accepted a duplicate key")
	}
}
