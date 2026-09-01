// VSCode-style binary channel protocol used between the phone UI and the
// desktop host. Wire format (reverse-engineered from the official clients):
//
//	value := tag byte followed by payload
//	  0 Undefined | 1 String (varint len + utf8) | 2 Buffer | 3 VSBuffer
//	  4 Array (varint count + values) | 5 Object (varint len + JSON utf8)
//	  6 Int (varint)
//
// Server→client messages: [200] Initialize, [201,id,data] PromiseSuccess,
// [202,id,data] PromiseError, [204,id,data] EventFire.
// Client→server messages: [100,id,channelName,name,arg] Promise,
// [102,id,channelName,name,arg] EventListen, [103,...] EventDispose,
// [101,id] PromiseCancel.
package relay

import (
	"encoding/json"
	"fmt"
)

const (
	chInitialize     = 200
	chPromiseSuccess = 201
	chPromiseError   = 202
	chPromiseErrObj  = 203
	chEventFire      = 204

	chPromise       = 100
	chPromiseCancel = 101
	chEventListen   = 102
	chEventDispose  = 103
)

// chWriter builds a serialized value.
type chWriter struct{ b []byte }

func (w *chWriter) byte(v byte) { w.b = append(w.b, v) }

func (w *chWriter) varint(v int) {
	u := uint64(v)
	for {
		if u < 0x80 {
			w.byte(byte(u))
			return
		}
		w.byte(byte(u&0x7f) | 0x80)
		u >>= 7
	}
}

func (w *chWriter) value(v any) {
	switch t := v.(type) {
	case nil:
		w.byte(0)
	case int:
		w.byte(6)
		w.varint(t)
	case int64:
		w.byte(6)
		w.varint(int(t))
	case float64:
		w.byte(6)
		w.varint(int(t))
	case bool:
		w.byte(6)
		if t {
			w.varint(1)
		} else {
			w.varint(0)
		}
	case string:
		w.byte(1)
		w.varint(len(t))
		w.b = append(w.b, []byte(t)...)
	case []byte:
		w.byte(3)
		w.varint(len(t))
		w.b = append(w.b, t...)
	case []any:
		w.byte(4)
		w.varint(len(t))
		for _, e := range t {
			w.value(e)
		}
	case map[string]any:
		b, _ := json.Marshal(t)
		w.byte(5)
		w.varint(len(b))
		w.b = append(w.b, b...)
	default:
		b, err := json.Marshal(v)
		if err == nil {
			w.byte(5)
			w.varint(len(b))
			w.b = append(w.b, b...)
			return
		}
		panic(fmt.Sprintf("channel: unsupported value %T", v))
	}
}

// chReader parses serialized values.
type chReader struct{ b []byte }

func (r *chReader) byte() byte {
	if len(r.b) == 0 {
		return 0
	}
	v := r.b[0]
	r.b = r.b[1:]
	return v
}

func (r *chReader) varint() int {
	var v uint64
	for i := uint(0); ; i += 7 {
		b := r.byte()
		v |= uint64(b&0x7f) << i
		if b&0x80 == 0 {
			return int(v)
		}
	}
}

func (r *chReader) value() any {
	switch tag := r.byte(); tag {
	case 0:
		return nil
	case 1:
		n := r.varint()
		if n > len(r.b) {
			n = len(r.b)
		}
		s := string(r.b[:n])
		r.b = r.b[n:]
		return s
	case 2, 3:
		n := r.varint()
		if n > len(r.b) {
			n = len(r.b)
		}
		buf := append([]byte{}, r.b[:n]...)
		r.b = r.b[n:]
		return buf
	case 4:
		n := r.varint()
		arr := make([]any, 0, n)
		for i := 0; i < n; i++ {
			arr = append(arr, r.value())
		}
		return arr
	case 5:
		n := r.varint()
		if n > len(r.b) {
			n = len(r.b)
		}
		s := string(r.b[:n])
		r.b = r.b[n:]
		return jsonRaw(s)
	case 6:
		return r.varint()
	default:
		return nil
	}
}

func jsonRaw(s string) any { return json.RawMessage(s) }

// buildMessage serializes [values...] + optional data, the wire form every
// channel message uses (serialize(head), serialize(data)).
func buildMessage(head []any, data any) []byte {
	w := &chWriter{}
	w.byte(4)
	w.varint(len(head))
	for _, v := range head {
		w.value(v)
	}
	w.value(data)
	return w.b
}

// initializeMessage is the server handshake: [200], data undefined.
func initializeMessage() []byte { return buildMessage([]any{chInitialize}, nil) }

// promiseSuccess wraps a JSON result for a client request id. The data is
// encoded as a native channel value (not a VSBuffer): the phone's service
// proxy hands `data` straight to the caller, so arrays must decode as arrays
// (tag 4), objects as JSON objects (tag 5), etc. — a VSBuffer would crash
// code that calls .find()/.map() on the result.
func promiseSuccess(id int, result []byte) []byte {
	w := &chWriter{}
	w.byte(4)
	w.varint(2)
	w.value(chPromiseSuccess)
	w.value(id)
	w.value(jsonToChannel(result))
	return w.b
}

// jsonToChannel parses JSON bytes into native channel values (array -> []any,
// object -> map[string]any, number -> int/float, bool, string, null).
func jsonToChannel(b []byte) any {
	var v any
	if err := json.Unmarshal(b, &v); err != nil {
		return json.RawMessage(b) // keep opaque bytes as an object-ish fallback
	}
	switch t := v.(type) {
	case map[string]any:
		return t
	case []any:
		return t
	case float64:
		if t == float64(int64(t)) {
			return int(t)
		}
		return t
	default:
		return v
	}
}

// ChannelCall is one decoded client→server request.
type ChannelCall struct {
	Kind        int // 100 promise | 102 event-listen | 103 event-dispose
	ID          int
	ChannelName string
	Name        string
	Arg         any
}

// parseChannelCall decodes a client binary message. The wire format is
// serialize(head=[kind,id,channel,name]) + serialize(data=[arg]): the arg is
// a separate array appended after the head array (buildMessage layout).
func parseChannelCall(b []byte) *ChannelCall {
	r := &chReader{b: b}
	arr, _ := r.value().([]any)
	if len(arr) == 0 {
		return nil
	}
	kind, _ := arr[0].(int)
	c := &ChannelCall{Kind: kind}
	if len(arr) > 1 {
		c.ID, _ = arr[1].(int)
	}
	if len(arr) > 2 {
		c.ChannelName, _ = arr[2].(string)
	}
	if len(arr) > 3 {
		c.Name, _ = arr[3].(string)
	}
	// The arg lives in the appended data value: Array[arg].
	if data, ok := r.value().([]any); ok && len(data) > 0 {
		c.Arg = data[0]
	}
	return c
}
