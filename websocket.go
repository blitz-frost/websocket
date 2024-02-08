//go:build !(wasm && js)

package websocket

import (
	"errors"
	stdio "io"
	"net/http"

	"github.com/blitz-frost/io"
	"github.com/blitz-frost/io/msg"
	"github.com/gorilla/websocket"
)

const (
	ChannelBinary = byte(websocket.BinaryMessage)
	ChannelText   = byte(websocket.TextMessage)
)

var (
	DefaultDialer   = &Dialer{}
	DefaultUpgrader = &Upgrader{
		CheckOrigin: func(r *http.Request) bool { return true },
	}
)

// Conn wraps a *gorilla/websocket.Conn to fit the msg package framework. Only binary and text messages are handled.
type Conn struct {
	V *websocket.Conn

	dstBinary msg.ReaderTaker
	dstText   msg.ReaderTaker

	closed bool // Close method call; shouldn't need to be atomic
}

func (x *Conn) Close() error {
	x.closed = true
	return x.V.Close()
}

// Listen routes incoming messages according to their type.
// If no target has been specified for a channel, the messages from that channel will be discarded.
// Returns on error, or when closed. In either case, the websocket is no longer usable. In particular, a chained ReaderTaker should not return an error when the connection should be kept alive.
//
// An active Listen loop is necessary in order to drain incoming messages.
func (x *Conn) Listen() (err error) {
	if x.dstBinary == nil {
		x.dstBinary = msg.Void{}
	}
	if x.dstText == nil {
		x.dstText = msg.Void{}
	}

	defer func() {
		if x.closed {
			// termination due to explicit Close call is not considered an error
			err = nil
		} else {
			// a ReaderTake error might leave the connection open
			// automatically close it
			x.V.Close()
		}
	}()

	var (
		ch int
		r  stdio.Reader
	)
	for {
		ch, r, err = x.V.NextReader()
		if err != nil {
			return
		}
		rc := io.ReaderOf(r)

		switch byte(ch) {
		case ChannelBinary:
			if err = x.dstBinary.ReaderTake(rc); err != nil {
				return
			}
		case ChannelText:
			if err = x.dstText.ReaderTake(rc); err != nil {
				return
			}
		}
	}
	return nil
}

// Should not be called during an active Listen loop.
func (x *Conn) ReaderChain(ch byte, dst msg.ReaderTaker) error {
	switch ch {
	case ChannelBinary:
		x.dstBinary = dst
	case ChannelText:
		x.dstText = dst
	default:
		return errors.New("invalid channel")
	}
	return nil
}

// Not concurrent safe.
func (x *Conn) Writer(ch byte) (msg.Writer, error) {
	switch ch {
	case ChannelBinary:
		fallthrough
	case ChannelText:
		w, err := x.V.NextWriter(int(ch))
		if err != nil {
			return nil, err
		}
		return w, nil
	}
	return nil, errors.New("invalid channel")
}

type Dialer websocket.Dialer

func (x *Dialer) Dial(url string) (*Conn, error) {
	conn, _, err := (*websocket.Dialer)(x).Dial(url, nil)
	if err != nil {
		return nil, err
	}
	return &Conn{
		V: conn,
	}, nil
}

type Upgrader websocket.Upgrader

func (x *Upgrader) Upgrade(w http.ResponseWriter, r *http.Request) (*Conn, error) {
	conn, err := (*websocket.Upgrader)(x).Upgrade(w, r, nil)
	if err != nil {
		return nil, err
	}
	return &Conn{
		V: conn,
	}, nil
}
