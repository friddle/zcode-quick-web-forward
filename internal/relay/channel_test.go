package relay

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
	// Real wire format: serialize(head=[kind,id,channel,name]) then
	// serialize(data=[arg]) — the arg is the first element of the appended
	// data array (buildMessage layout the phone uses).
	w := &chWriter{}
	w.byte(4)
	w.varint(4) // head array: 4 elements
	for _, v := range []any{100, 7, "chan", "method"} {
		w.value(v)
	}
	w.byte(4)
	w.varint(1) // data array: 1 element
	w.value("arg")
	c := parseChannelCall(w.b)
	if c == nil || c.Kind != 100 || c.ID != 7 || c.ChannelName != "chan" || c.Name != "method" || c.Arg != "arg" {
		t.Fatalf("parsed = %+v", c)
	}
}

func TestPromiseSuccessBytes(t *testing.T) {
	// PromiseSuccess data is encoded as a native channel value, NOT a VSBuffer:
	// the phone's service proxy passes `data` straight to the caller, so
	// `{"ok":true}` must decode as an object (tag 5) so .find()/.map() work.
	got := promiseSuccess(3, []byte(`{"ok":true}`))
	r := &chReader{b: got}
	arr, _ := r.value().([]any)
	if len(arr) != 2 || arr[0] != 201 || arr[1] != 3 {
		t.Fatalf("head = %v", arr)
	}
	// data follows as native JSON object value (tag 5)
	if r.byte() != 5 {
		t.Fatalf("data tag = %d, want 5 (native object)", r.b[0])
	}
}

func TestPromiseSuccessArray(t *testing.T) {
	// Array results must decode as channel arrays (tag 4) so the phone can
	// call .find()/.map() on them — the model-provider crash was exactly this.
	got := promiseSuccess(3, []byte(`[]`))
	r := &chReader{b: got}
	_, _ = r.value().([]any) // head
	if tag := r.byte(); tag != 4 {
		t.Fatalf("empty-array data tag = %d, want 4 (native array)", tag)
	}
	got2 := promiseSuccess(4, []byte(`[1,2]`))
	r2 := &chReader{b: got2}
	_, _ = r2.value().([]any)
	if tag := r2.byte(); tag != 4 {
		t.Fatalf("array data tag = %d, want 4", tag)
	}
}
