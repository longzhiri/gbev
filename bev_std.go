// +build !linux,!darwin,!netbsd,!freebsd,!openbsd,!dragonfly std

package gbev

import (
	"context"
	"math"
	"net"
	"sync"
)

type BufevCallback interface {
	OnRead(be *Bufev)
	OnWrite(be *Bufev)
	OnFree(be *Bufev, err error)
}

type Bufev struct {
	eb *EventBase
	cb BufevCallback

	conn net.Conn

	ctx   context.Context
	quitF context.CancelFunc

	mu    sync.Mutex
	freed bool

	rmu                    sync.Mutex
	inBuf                  []byte
	rInOff, wInoff, lInoff int // inbuf's read off, write off, lock off
	rsignal                chan struct{}
	maxInBufLen            int
	singleReadSz           int
	rpause                 bool
	rEnable                bool

	wmu             sync.Mutex
	outBufQueue     [][]byte
	swapOutBufQueue [][]byte
	wpause          bool
	wsignal         chan struct{}
	outBufTotalLen  int
	maxOutBufLen    int
}

func newBufev(eb *EventBase, conn net.Conn, maxInBufLen int, singleReadSz int, maxOutBufLen int, cb BufevCallback) *Bufev {
	ctx, quitF := context.WithCancel(context.Background())
	be := &Bufev{
		eb:           eb,
		cb:           cb,
		conn:         conn,
		maxInBufLen:  maxInBufLen,
		singleReadSz: singleReadSz,
		maxOutBufLen: maxOutBufLen,
		ctx:          ctx,
		quitF:        quitF,
		rsignal:      make(chan struct{}, 1),
		inBuf:        make([]byte, maxInBufLen),
		wsignal:      make(chan struct{}, 1),
	}
	go be.readLoop()
	go be.writeLoop()
	return be
}

func (be *Bufev) readLoop() {
	var err error
	defer be.eb.freeBufevWithError(be, err)

	for {
		be.rmu.Lock()
		leftCap := len(be.inBuf) - be.wInoff
		nr := be.singleReadSz
		if nr > leftCap {
			nr = leftCap
		}
		var rpause bool
		if nr == 0 || !be.rEnable {
			be.rpause = true
			rpause = true
		} else {
			be.lInoff = be.wInoff + nr
		}
		wInoff, lInoff := be.wInoff, be.lInoff
		be.rmu.Unlock()

		if rpause {
			select {
			case <-be.rsignal:
			case <-be.ctx.Done():
				return
			}
			continue
		}

		var n int
		n, err = be.conn.Read(be.inBuf[wInoff:lInoff])
		if err != nil {
			return
		}

		be.rmu.Lock()
		be.wInoff += n
		be.lInoff = 0
		be.rmu.Unlock()

		be.cb.OnRead(be)

		be.rmu.Lock()
		if be.rInOff > 0 {
			copy(be.inBuf[:be.wInoff-be.rInOff], be.inBuf[be.rInOff:be.wInoff])
			be.wInoff = be.wInoff - be.rInOff
			be.rInOff = 0
		}
		be.rmu.Unlock()
	}
}

func (be *Bufev) BufevRead(data []byte, peek bool) (n int, err error) {
	be.rmu.Lock()
	n = copy(data, be.inBuf[be.rInOff:be.wInoff])
	if !peek && n > 0 {
		be.rInOff += n
		if be.lInoff == 0 {
			copy(be.inBuf[:be.wInoff-be.rInOff], be.inBuf[be.rInOff:be.wInoff])
			be.wInoff = be.wInoff - be.rInOff
			be.rInOff = 0
		}
		if be.rpause {
			be.rpause = false
			select {
			case be.rsignal <- struct{}{}:
			default:
			}
		}
	}
	be.rmu.Unlock()
	return
}

func (be *Bufev) InBufLen() int {
	be.rmu.Lock()
	l := be.wInoff - be.rInOff
	be.rmu.Unlock()
	return l
}

func (be *Bufev) FD() int {
	return 0
}

func (be *Bufev) writeLoop() {
	var err error
	defer be.eb.freeBufevWithError(be, err)

	for {
		be.wmu.Lock()
		writingBufQueue := be.outBufQueue
		writingTotalLen := be.outBufTotalLen
		if len(writingBufQueue) != 0 {
			be.outBufQueue, be.swapOutBufQueue = be.swapOutBufQueue, nil
			be.outBufTotalLen = 0
		} else {
			be.wpause = true
		}
		be.wmu.Unlock()

		if len(writingBufQueue) == 0 {
			select {
			case <-be.wsignal:
			case <-be.ctx.Done():
				return
			}
			continue
		}
		swapOutBufQueue := writingBufQueue[:0]
		bufferWriter := net.Buffers(writingBufQueue)
		for writingTotalLen > 0 {
			var n int64
			n, err = bufferWriter.WriteTo(be.conn)
			if err != nil {
				return
			}
			writingTotalLen -= int(n)
		}

		be.wmu.Lock()
		be.swapOutBufQueue = swapOutBufQueue
		be.wmu.Unlock()

		be.cb.OnWrite(be)
	}
}

func (be *Bufev) BufevWrite(data []byte) (n int, err error) {
	if len(data) == 0 {
		return 0, nil
	}
	be.wmu.Lock()
	if int32(len(data)) > math.MaxInt32-int32(be.outBufTotalLen) {
		be.wmu.Unlock()
		return 0, ErrWriteBufferLimitReached
	}
	be.outBufTotalLen += len(data)
	if be.outBufTotalLen > be.maxOutBufLen {
		clipBytes := be.outBufTotalLen - be.maxOutBufLen
		be.outBufTotalLen = be.maxOutBufLen
		n = len(data) - clipBytes
		data = data[:n]
		err = ErrWriteBufferLimitReached
	} else {
		n = len(data)
	}
	if len(data) > 0 {
		be.outBufQueue = append(be.outBufQueue, data)
		if be.wpause {
			be.wpause = false
			select {
			case be.wsignal <- struct{}{}:
			default:
			}
		}
	}
	be.wmu.Unlock()
	return
}

func (be *Bufev) free(err error) {
	be.mu.Lock()
	if be.freed {
		be.mu.Unlock()
		return
	}
	be.freed = true
	be.quitF()
	be.conn.Close()
	be.mu.Unlock()

	be.cb.OnFree(be, err)
}

func (be *Bufev) Conn() net.Conn {
	return be.conn
}

func (be *Bufev) SetReadEnable(enable bool) error {
	be.rmu.Lock()
	defer be.rmu.Unlock()
	if be.rEnable == enable {
		return nil
	}

	be.rEnable = enable

	if enable {
		if be.rpause {
			be.rpause = false
			select {
			case be.rsignal <- struct{}{}:
			default:
			}
		}
	}
	return nil
}

type EventBase struct {
	mu         sync.Mutex
	conn2Bufev map[net.Conn]*Bufev
	closed     bool
}

func NewEventBase() (*EventBase, error) {
	return &EventBase{
		conn2Bufev: make(map[net.Conn]*Bufev),
	}, nil
}

func (eb *EventBase) NewBufev(conn net.Conn, maxInBufLen, singleReadSz int, maxOutBufLen int, cb BufevCallback) (*Bufev, error) {
	eb.mu.Lock()
	defer eb.mu.Unlock()

	if eb.closed {
		return nil, ErrEvtBaseClosed
	}

	if eb.conn2Bufev[conn] != nil {
		return nil, ErrConnAlreadyBound
	}

	if singleReadSz > maxInBufLen {
		singleReadSz = maxInBufLen
	}
	be := newBufev(eb, conn, maxInBufLen, singleReadSz, maxOutBufLen, cb)
	eb.conn2Bufev[conn] = be
	return be, nil
}

func (eb *EventBase) FreeBufev(be *Bufev) {
	eb.freeBufevWithError(be, nil)
}

func (eb *EventBase) freeBufevWithError(be *Bufev, err error) {
	eb.mu.Lock()
	delete(eb.conn2Bufev, be.conn)
	eb.mu.Unlock()
	be.free(err)
}

func (eb *EventBase) Close() {
	bes := make(map[net.Conn]*Bufev)

	eb.mu.Lock()
	if eb.closed {
		eb.mu.Unlock()
		return
	}
	eb.closed = true
	for conn, be := range eb.conn2Bufev {
		bes[conn] = be
	}
	eb.mu.Unlock()

	for _, be := range bes {
		eb.freeBufevWithError(be, ErrEvtBaseClosed)
	}
}
