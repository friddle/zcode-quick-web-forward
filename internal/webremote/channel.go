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
package webremote

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
	case string:
		w.byte(1)
		w.varint(len(t))
		w.b = append(w.b, []byte(t)...)
	case []any:
		w.byte(4)
		w.varint(len(t))
		for _, e := range t {
			w.value(e)
		}
	default:
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

// promiseSuccess wraps a JSON result for a client request id. The data is a
// VSBuffer value (tag 3) so arbitrary JSON bytes survive verbatim.
func promiseSuccess(id int, result []byte) []byte {
	w := &chWriter{}
	w.byte(4)
	w.varint(2)
	w.value(chPromiseSuccess)
	w.value(id)
	// data: VSBuffer(result)
	w.byte(3)
	w.varint(len(result))
	w.b = append(w.b, result...)
	return w.b
}

// ChannelCall is one decoded client→server request.
type ChannelCall struct {
	Kind        int // 100 promise | 102 event-listen | 103 event-dispose
	ID          int
	ChannelName string
	Name        string
	Arg         any
}

// parseChannelCall decodes a client binary message.
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
	if len(arr) > 4 {
		c.Arg = arr[4]
	}
	return c
}
