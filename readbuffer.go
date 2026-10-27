package rdgw

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"sync"
	"time"

	"github.com/sirupsen/logrus"
)

const (
	MaxBuffer      = 1 << 21
	maxBufferLimit = MaxBuffer * 2
)

var errReadBufferLimit = fmt.Errorf("remotedialer read buffer exceeded %d bytes", maxBufferLimit)

type readBuffer struct {
	id, readCount, offerCount int64
	cond                      sync.Cond
	deadline                  time.Time
	buf                       bytes.Buffer
	err                       error
	backPressure              *backPressure
}

func newReadBuffer(id int64, backPressure *backPressure) *readBuffer {
	return &readBuffer{
		id:           id,
		backPressure: backPressure,
		cond: sync.Cond{
			L: &sync.Mutex{},
		},
	}
}

func (r *readBuffer) Status() string {
	r.cond.L.Lock()
	defer r.cond.L.Unlock()
	return fmt.Sprintf("%d/%d", r.readCount, r.offerCount)
}

func (r *readBuffer) SetDeadline(deadline time.Time) {
	r.cond.L.Lock()
	r.deadline = deadline
	r.cond.Broadcast()
	r.cond.L.Unlock()
}

func (r *readBuffer) Offer(reader io.Reader) error {
	var shouldPause bool
	var offerErr error
	func() {
		r.cond.L.Lock()
		defer r.cond.L.Unlock()

		if r.err != nil {
			offerErr = r.err
			return
		}

		remaining := int64(maxBufferLimit - r.buf.Len())
		if remaining < 0 {
			offerErr = errReadBufferLimit
			return
		}

		// 只向内存缓冲写入剩余容量，并额外探测一个字节判断是否超限。
		n, err := io.Copy(&r.buf, io.LimitReader(reader, remaining))
		r.offerCount += n
		if err != nil {
			offerErr = err
			return
		}

		if n == remaining {
			var extra [1]byte
			extraN, extraErr := io.ReadFull(reader, extra[:])
			if extraN > 0 {
				offerErr = errReadBufferLimit
				return
			}
			if extraErr != nil && !errors.Is(extraErr, io.EOF) && !errors.Is(extraErr, io.ErrUnexpectedEOF) {
				offerErr = extraErr
				return
			}
		}

		if n > 0 {
			r.cond.Broadcast()
		}
		shouldPause = r.buf.Len() > MaxBuffer
	}()
	if offerErr != nil {
		return offerErr
	}
	if shouldPause {
		return r.backPressure.Pause()
	}
	return nil
}

func (r *readBuffer) Read(b []byte) (int, error) {
	for {
		r.cond.L.Lock()
		if r.buf.Len() > 0 {
			n, err := r.buf.Read(b)
			if err != nil {
				// bytes.Buffer 在 Len() > 0 时通常不会返回错误；若内部不变量被破坏，
				// 将错误限制在当前连接，避免整个进程因单条隧道崩溃。
				readErr := fmt.Errorf("read buffer invariant violated: %w", err)
				r.err = readErr
				r.cond.Broadcast()
				r.cond.L.Unlock()
				return 0, readErr
			}
			r.readCount += int64(n)
			r.cond.Broadcast()
			shouldResume := r.buf.Len() < MaxBuffer/8
			r.cond.L.Unlock()
			if shouldResume {
				if err := r.backPressure.Resume(); err != nil {
					return n, err
				}
			}
			return n, nil
		}

		if r.buf.Cap() > MaxBuffer/8 {
			logrus.Debugf("resetting remotedialer buffer id=%d to zero, old cap %d", r.id, r.buf.Cap())
			r.buf = bytes.Buffer{}
		}

		if r.err != nil {
			r.cond.L.Unlock()
			return 0, r.err
		}

		now := time.Now()
		if !r.deadline.IsZero() {
			if now.After(r.deadline) {
				r.cond.L.Unlock()
				return 0, errors.New("deadline exceeded")
			}
		}

		var t *time.Timer
		if !r.deadline.IsZero() {
			t = time.AfterFunc(r.deadline.Sub(now), func() {
				r.cond.L.Lock()
				r.cond.Broadcast()
				r.cond.L.Unlock()
			})
		}
		r.cond.Wait()
		if t != nil {
			t.Stop()
		}
		r.cond.L.Unlock()
	}
}

func (r *readBuffer) Close(err error) error {
	r.cond.L.Lock()
	defer r.cond.L.Unlock()
	if r.err == nil {
		r.err = err
	}
	r.cond.Broadcast()
	return nil
}
