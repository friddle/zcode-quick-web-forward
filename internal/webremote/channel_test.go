package webremote

import (
	"bytes"
	"encoding/hex"
	"testing"
)

func TestInitializeMessageBytes(t *testing.T) {
	got := initializeMessage()
	// Array(1) Int(200) Undefined
	want, _ := hex.DecodeString("040106c80100")
	if !bytes.Equal(got, want) {
		t.Fatalf("initialize = %x, want %x", got, want)
	}
}

func TestRoundTripChannelCall(t *testing.T) {
	// client promise: [100, 7, "chan", "method", "arg"]
	w := &chWriter{}
	w.byte(4)
	w.varint(5)
	for _, v := range []any{100, 7, "chan", "method", "arg"} {
		w.value(v)
	}
	w.value(nil)
	c := parseChannelCall(w.b)
	if c == nil || c.Kind != 100 || c.ID != 7 || c.ChannelName != "chan" || c.Name != "method" || c.Arg != "arg" {
		t.Fatalf("parsed = %+v", c)
	}
}

func TestPromiseSuccessBytes(t *testing.T) {
	got := promiseSuccess(3, []byte(`{"ok":true}`))
	r := &chReader{b: got}
	arr, _ := r.value().([]any)
	if len(arr) != 2 || arr[0] != 201 || arr[1] != 3 {
		t.Fatalf("head = %v", arr)
	}
	// data follows as VSBuffer tag 3
	if r.byte() != 3 {
		t.Fatalf("data tag = %d", r.b[0])
	}
}
