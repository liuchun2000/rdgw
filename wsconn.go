package remotedialer

import (
	"context"
	"fmt"
	"io"
	"time"

	"github.com/gorilla/websocket"
)

type wsWrapper struct {
	// writeMu 保护并发 WebSocket 写入，并支持按 deadline 获取。
	writeMu chan struct{}
	// conn is the underlying websocket connection
	conn *websocket.Conn
}

func newWSConn(conn *websocket.Conn) *wsWrapper {
	w := &wsWrapper{
		conn:    conn,
		writeMu: make(chan struct{}, 1),
	}
	w.writeMu <- struct{}{}
	conn.SetReadLimit(maxBufferLimit)
	w.setupDeadline()
	return w
}

type wsConn interface {
	// Close will indicate the underlying websocket connection
	Close() error
	// NextReader gets a new reader from the underlying websocket connection
	NextReader() (int, io.Reader, error)
	// WriteControl writes a new websocket control frame, see https://datatracker.ietf.org/doc/html/rfc6455#section-5.5
	WriteControl(messageType int, deadline time.Time, data []byte) error
	// WriteMessage writes a new websocket data frame, see https://datatracker.ietf.org/doc/html/rfc6455#section-6
	WriteMessage(messageType int, deadline time.Time, data []byte) error
}

func (w *wsWrapper) WriteControl(messageType int, deadline time.Time, data []byte) error {
	if err := w.acquireWrite(deadline); err != nil {
		return err
	}
	defer w.releaseWrite()

	return w.conn.WriteControl(messageType, data, deadline)
}

func (w *wsWrapper) WriteMessage(messageType int, deadline time.Time, data []byte) error {
	if err := w.acquireWrite(deadline); err != nil {
		return err
	}
	defer w.releaseWrite()

	if err := w.conn.SetWriteDeadline(deadline); err != nil {
		return err
	}
	return w.conn.WriteMessage(messageType, data)
}

func (w *wsWrapper) acquireWrite(deadline time.Time) error {
	if deadline.IsZero() {
		<-w.writeMu
		return nil
	}

	ctx, cancel := context.WithDeadline(context.Background(), deadline)
	defer cancel()
	select {
	case <-w.writeMu:
		return nil
	case <-ctx.Done():
		return fmt.Errorf("i/o timeout")
	}
}

func (w *wsWrapper) releaseWrite() {
	w.writeMu <- struct{}{}
}

func (w *wsWrapper) NextReader() (int, io.Reader, error) {
	return w.conn.NextReader()
}

func (w *wsWrapper) Close() error {
	return w.conn.Close()
}

func (w *wsWrapper) setupDeadline() {
	w.conn.SetReadDeadline(time.Now().Add(PingWaitDuration))
	w.conn.SetPingHandler(func(string) error {
		deadline := time.Now().Add(PingWaitDuration)
		if err := w.acquireWrite(deadline); err != nil {
			return err
		}
		err := w.conn.WriteControl(websocket.PongMessage, []byte(""), deadline)
		w.releaseWrite()
		if err != nil {
			return err
		}
		if err := w.conn.SetReadDeadline(time.Now().Add(PingWaitDuration)); err != nil {
			return err
		}
		return w.conn.SetWriteDeadline(time.Now().Add(PingWaitDuration))
	})
	w.conn.SetPongHandler(func(string) error {
		if err := w.conn.SetReadDeadline(time.Now().Add(PingWaitDuration)); err != nil {
			return err
		}
		return w.conn.SetWriteDeadline(time.Now().Add(PingWaitDuration))
	})

}
