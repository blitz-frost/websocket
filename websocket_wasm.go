//go:build wasm && js

// Package websocket wraps the Javascript Websocket API.
package websocket

import (
	"errors"
	"fmt"
	"syscall/js"
	"time"

	"github.com/blitz-frost/io"
	"github.com/blitz-frost/io/msg"
	"github.com/blitz-frost/wasm"
)

const (
	ChannelBinary = 2
	ChannelText   = 1
)

const (
	CodeNormal          Code = 1000
	CodeGoingAway            = 1001
	CodeProtocolError        = 1002
	CodeUnsupported          = 1003
	CodeReserved             = 1004
	CodeNoStatus             = 1005
	CodeAbnormal             = 1006
	CodeInvalidFrame         = 1007
	CodePolicyViolation      = 1008
	CodeTooBig               = 1009
	CodeMandatory            = 1010
	CodeInternalError        = 1011
	CodeRestart              = 1012
	CodeTryAgain             = 1013
	CodeBadGateway           = 1014
	CodeTLS                  = 1015
)

var DefaultDialer = &Dialer{
	ReadBufferSize:  4096,
	WriteBufferSize: 4096,
}

var (
	global      = js.Global()
	textDecoder = global.Get("TextDecoder").New()
	textEncoder = global.Get("TextEncoder").New()
)

type Code int

type Conn struct {
	V js.Value // JS websocket object

	// JS barrier traversal
	wJs wasm.BytesWriter

	// buffer data transfer to and from JS, minimizing slow calls
	wBuf *io.WriteBuffer // writes to wJs
	rBuf *io.ReadBuffer  // reads from current message

	closeHandler js.Func
	closeFunc    func(Code)
	closeChan    chan struct{}

	msgHandler js.Func
	dstBinary  msg.ReaderTaker
	dstText    msg.ReaderTaker
}

func (x *Conn) Close() error {
	x.V.Call("close")
	return nil
}

// OnClose registers fn to be called with the closing code, when the websocket closes.
func (x *Conn) OnClose(fn func(Code)) {
	x.closeFunc = fn
}

// Listen is only present for consistency with the non-wasm version. Will return on websocket close.
func (x *Conn) Listen() error {
	<-x.closeChan
	return nil
}

func (x *Conn) ReaderChain(ch byte, rt msg.ReaderTaker) error {
	switch ch {
	case ChannelBinary:
		x.dstBinary = rt
	case ChannelText:
		x.dstText = rt
	default:
		return errors.New("invalid channel")
	}
	return nil
}

// Wait blocks until the currently buffered data is written out, or the websocket closes.
// Since the JS API doesn't have an event for this, the current buffer amount is checked every d time.
func (x *Conn) Wait(d time.Duration) {
	for {
		if x.V.Get("readyState").Int() == 3 {
			return
		}

		if x.V.Get("bufferedAmount").Int() == 0 {
			return
		}

		time.Sleep(d)
	}
}

// Not concurrent safe.
func (x *Conn) Writer(ch byte) (msg.Writer, error) {
	return writer{
		v:  x.V,
		b:  &x.wJs.Dst,
		w:  x.wBuf,
		ch: ch,
	}, nil
}

type Dialer struct {
	ReadBufferSize  int // controls minimum read size from JS to Go
	WriteBufferSize int // controls minimum write size from Go to JS
}

// Dial opens a new websocket connection.
func (x *Dialer) Dial(url string) (*Conn, error) {
	ws := global.Get("WebSocket").New(url)
	// TODO check if actually connected
	ws.Set("binaryType", "arraybuffer") // requires ones less async function call to read

	wBytes := wasm.MakeBytes(0, x.WriteBufferSize)

	o := Conn{
		V:         ws,
		wJs:       wasm.BytesWriter{wBytes},
		closeChan: make(chan struct{}),
	}
	o.wBuf = io.WriteBufferNew(&o.wJs, make([]byte, x.WriteBufferSize))
	o.rBuf = io.ReadBufferNew(&wasm.BytesReader{}, make([]byte, x.ReadBufferSize))

	// hook close event
	o.closeFunc = func(c Code) { fmt.Println("websocket closed", c) }
	o.closeHandler = js.FuncOf(func(this js.Value, args []js.Value) interface{} {
		code := Code(args[0].Get("code").Int())
		o.closeFunc(code)
		close(o.closeChan)

		// cleanup
		o.msgHandler.Release()
		o.closeHandler.Release()

		return nil
	})
	ws.Set("onclose", o.closeHandler)

	// hook message event
	o.dstBinary = msg.Void{}
	o.dstText = msg.Void{}
	o.msgHandler = js.FuncOf(func(this js.Value, args []js.Value) interface{} {
		data := args[0].Get("data")
		var (
			buf wasm.Bytes
			dst msg.ReaderTaker
		)
		if data.Type() == js.TypeString {
			bufJs := textEncoder.Call("encode", data)
			buf = wasm.View(bufJs)
			dst = o.dstText
		} else {
			buf = wasm.View(data)
			dst = o.dstBinary
		}
		r := wasm.BytesReader{buf}
		o.rBuf.Reset(&r)
		dst.ReaderTake(o.rBuf)

		return nil
	})
	ws.Set("onmessage", o.msgHandler)

	// wait for connection
	ch := make(chan struct{})
	fn := js.FuncOf(func(this js.Value, args []js.Value) interface{} {
		ch <- struct{}{}
		return nil
	})
	ws.Set("onopen", fn)

	<-ch
	fn.Release()
	return &o, nil
}

type writer struct {
	v  js.Value
	b  *wasm.Bytes     // JS buffer
	w  *io.WriteBuffer // buffered writer to b
	ch byte
}

func (x writer) Close() error {
	err := x.w.Flush()
	if err != nil {
		return err
	}

	buf := x.b.Js()
	switch x.ch {
	case ChannelBinary:
		x.v.Call("send", buf)
	case ChannelText:
		// to send text with the JS API, we have to call "send" with a string value
		str := textDecoder.Call("decode", buf)
		x.v.Call("send", str)
	}

	*x.b = x.b.Slice(0, 0)
	return nil
}

func (x writer) Write(b []byte) (int, error) {
	return x.w.Write(b)
}
