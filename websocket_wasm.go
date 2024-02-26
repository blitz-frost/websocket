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
	CodeGoingAway       Code = 1001
	CodeProtocolError   Code = 1002
	CodeUnsupported     Code = 1003
	CodeReserved        Code = 1004
	CodeNoStatus        Code = 1005
	CodeAbnormal        Code = 1006
	CodeInvalidFrame    Code = 1007
	CodePolicyViolation Code = 1008
	CodeTooBig          Code = 1009
	CodeMandatory       Code = 1010
	CodeInternalError   Code = 1011
	CodeRestart         Code = 1012
	CodeTryAgain        Code = 1013
	CodeBadGateway      Code = 1014
	CodeTLS             Code = 1015
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

type CloseError struct {
	Code Code
}

func (x CloseError) Error() string {
	return fmt.Sprint("code %i", x.Code)
}

type Code int

type Conn struct {
	V js.Value // JS websocket object

	// JS barrier traversal
	wJs wasm.BytesWriter

	// buffer data transfer to and from JS, minimizing slow calls
	wBuf *io.WriteBuffer // writes to wJs
	rBuf *io.ReadBuffer  // reads from current message

	closeHandler js.Func
	closeChan    chan error
	closed       bool // explicit Close call

	errorHandler js.Func

	msgHandler js.Func
	dstBinary  msg.ReaderTaker
	dstText    msg.ReaderTaker
}

func (x *Conn) Close() error {
	x.closed = true
	x.V.Call("close")
	x.closeChan <- nil
	return nil
}

// Listen begins accepting incoming messages.
// Returns on error or explicit Close call.
func (x *Conn) Listen() error {
	if x.dstBinary == nil {
		x.dstBinary = msg.Void{}
	}
	if x.dstText == nil {
		x.dstText = msg.Void{}
	}

	x.msgHandler = js.FuncOf(func(this js.Value, args []js.Value) interface{} {
		data := args[0].Get("data")
		var (
			buf wasm.Bytes
			dst msg.ReaderTaker
		)
		if data.Type() == js.TypeString {
			bufJs := textEncoder.Call("encode", data)
			buf = wasm.View(bufJs)
			dst = x.dstText
		} else {
			buf = wasm.View(data)
			dst = x.dstBinary
		}
		r := wasm.BytesReader{buf}
		x.rBuf.Reset(&r)
		err := dst.ReaderTake(x.rBuf)
		if err != nil {
			x.V.Call("close")
			x.closeChan <- err
		}

		return nil
	})
	x.V.Set("onmessage", x.msgHandler)

	err := <-x.closeChan
	if !x.closed {
		return err
	}
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

func (x *Conn) cleanup() {
	x.msgHandler.Release()
	x.closeHandler.Release()
	x.errorHandler.Release()
}

type Dialer struct {
	ReadBufferSize  int // controls minimum read size from JS to Go
	WriteBufferSize int // controls minimum write size from Go to JS
}

// Dial opens a new websocket connection.
func (x *Dialer) Dial(url string) (*Conn, error) {
	ws := global.Get("WebSocket").New(url)

	ws.Set("binaryType", "arraybuffer") // requires ones less async function call to read

	wBytes := wasm.BytesMake(0, x.WriteBufferSize)

	o := Conn{
		V:         ws,
		wJs:       wasm.BytesWriter{wBytes},
		closeChan: make(chan error, 2),
	}
	o.wBuf = io.WriteBufferMake(&o.wJs, make([]byte, x.WriteBufferSize))
	o.rBuf = io.ReadBufferMake(&wasm.BytesReader{}, make([]byte, x.ReadBufferSize))

	// hook close event
	o.closeHandler = js.FuncOf(func(this js.Value, args []js.Value) interface{} {
		code := Code(args[0].Get("code").Int())
		o.closeChan <- CloseError{code}

		o.cleanup()

		return nil
	})
	ws.Set("onclose", o.closeHandler)

	// failing to connect will issue an error event
	// the close handler will also be called afterwards
	errorChan := make(chan struct{})
	o.errorHandler = js.FuncOf(func(this js.Value, args []js.Value) interface{} {
		close(errorChan)
		return nil
	})
	ws.Set("onerror", o.errorHandler)

	// wait for connection
	ch := make(chan struct{}, 1)
	fn := js.FuncOf(func(this js.Value, args []js.Value) interface{} {
		ch <- struct{}{}
		return nil
	})
	ws.Set("onopen", fn)
	defer fn.Release()

	select {
	case <-ch:
	case <-errorChan:
		return nil, errors.New("connection failed")
	}

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
