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
	textDecoder = wasm.Global.Get("TextDecoder").New()
	textEncoder = wasm.Global.Get("TextEncoder").New()
)

type CloseError struct {
	Code Code
}

func (x CloseError) Error() string {
	return fmt.Sprint("code %i", x.Code)
}

type Code int

type Conn struct {
	v wasm.Value // JS websocket object

	dialer Dialer

	closeChan chan error
	closeFn   wasm.DynamicFunction
	closed    bool // explicit Close call

	errorFn wasm.DynamicFunction

	wJs  wasm.BytesWriter // JS barrier traversal
	wBuf *io.WriteBuffer  // writes to wJs; buffers data in order to minimize slow writes
}

func connMake(url string, dialer Dialer) (*Conn, error) {
	ws := wasm.Global.Get("WebSocket").New(url)
	ws.Set("binaryType", "arraybuffer") // requires ones less async function call to read

	wBytes := wasm.BytesMake(0, dialer.WriteBufferSize)

	o := Conn{
		v:         ws,
		wJs:       wasm.BytesWriter{wBytes},
		closeChan: make(chan error, 2), // 1 for Close(), 1 for closeFn
		dialer:    dialer,
	}
	o.wBuf = io.WriteBufferMake(&o.wJs, make([]byte, dialer.WriteBufferSize))

	// hook close event
	var inter wasm.InterfaceFunc
	inter = func(this wasm.Value, args []wasm.Value) (wasm.Any, error) {
		code := Code(args[0].Get("code").Int())
		o.closeChan <- CloseError{code}

		o.wipe()

		return nil, nil
	}
	o.closeFn.Remake(inter)
	ws.Set("onclose", o.closeFn.Value())

	// failing to connect will issue an error event
	// the close handler will also be called afterwards
	errorChan := make(chan struct{})
	inter = func(this wasm.Value, args []wasm.Value) (wasm.Any, error) {
		close(errorChan)
		return nil, nil
	}
	o.errorFn.Remake(inter)
	ws.Set("onerror", o.errorFn.Value())

	// wait for connection
	ch := make(chan struct{}, 1)
	inter = func(this wasm.Value, args []wasm.Value) (wasm.Any, error) {
		close(ch)
		return nil, nil
	}
	openFn := wasm.DynamicFunctionMake(inter)
	ws.Set("onopen", openFn.Value())
	defer openFn.Wipe()

	select {
	case <-ch:
	case <-errorChan:
		return nil, errors.New("connection failed")
	}

	return &o, nil
}

func (x *Conn) Close() error {
	x.closed = true
	x.v.Call("close")
	x.closeChan <- nil
	return nil
}

// Registers a Function to accept incoming messages. Returns on error or explicit Close().
//
// fn - Function({"data": data}); data - string from text channel, ArrayBuffer from binary channel
func (x *Conn) Listen(fn wasm.Function) error {
	x.v.Set("onmessage", fn.Value())

	err := <-x.closeChan
	if !x.closed {
		return err
	}
	return nil
}

// The returned value is specific for this Conn.
func (x *Conn) ListenInterface() *Listener {
	return listenerMake(x)
}

// Wait blocks until the currently buffered data is written out, or the websocket closes.
// Since the JS API doesn't have an event for this, the current buffer amount is checked every d time.
//
// Must not be called from the event loop.
func (x *Conn) Wait(d time.Duration) {
	for {
		if x.v.Get("readyState").Int() == 3 {
			return
		}

		if x.v.Get("bufferedAmount").Int() == 0 {
			return
		}

		time.Sleep(d)
	}
}

// Not concurrent safe.
func (x *Conn) Writer(ch byte) (msg.Writer, error) {
	return writer{
		v:  x.v,
		b:  &x.wJs.Dst,
		w:  x.wBuf,
		ch: ch,
	}, nil
}

func (x *Conn) wipe() {
	x.closeFn.Wipe()
	x.errorFn.Wipe()
}

type Dialer struct {
	ReadBufferSize  int // controls minimum read size from JS to Go
	WriteBufferSize int // controls minimum write size from Go to JS
}

func (x Dialer) Dial(url string) (*Conn, error) {
	return connMake(url, x)
}

type Listener struct {
	conn *Conn

	buf *io.ReadBuffer // reads from current message

	dstBinary msg.ReaderTaker
	dstText   msg.ReaderTaker
}

func listenerMake(conn *Conn) *Listener {
	return &Listener{
		conn:      conn,
		buf:       io.ReadBufferMake(&wasm.BytesReader{}, make([]byte, conn.dialer.ReadBufferSize)),
		dstBinary: msg.Void{},
		dstText:   msg.Void{},
	}
}

// Not safe to call while the Listener is in use.
func (x *Listener) ReaderChain(ch byte, rt msg.ReaderTaker) error {
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

// Directs incoming messages according to their respective channel setup.
// Will close the corresponding Conn if a ReaderTaker returns an error.
func (x *Listener) Exec(this wasm.Value, args []wasm.Value) (wasm.Any, error) {
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
	x.buf.Reset(&r)
	err := dst.ReaderTake(x.buf)
	if err != nil {
		x.conn.v.Call("close")
		x.conn.closeChan <- err
	}

	return nil, nil
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

	buf := x.b.Value()
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
