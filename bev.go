// +build linux darwin netbsd freebsd openbsd dragonfly

package gbev

import (
	"errors"
	"io"
	"math"
	"net"
	"os"
	"sync"
	"syscall"
	"unsafe"
)

const (
	EV_READ = 1 << iota
	EV_WRITE
)

type pollEvent struct {
	fd  int
	evs uint32
}

var (
	ErrUnsupported             = errors.New("unsupported connection type")
	ErrConnAlreadyBound        = errors.New("connection already bound")
	ErrWriteBufferLimitReached = errors.New("write buffer limit reached")
	ErrBufevFreed              = errors.New("bufev already freed")
	ErrPollerClosed            = errors.New("poller closed")
	ErrEvtBaseClosed           = errors.New("event base closed")
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
	fd   int
	file *os.File

	mu      sync.Mutex
	evs     uint32
	rEnable bool
	rPause  bool
	freed   bool

	rmu          sync.Mutex
	inBuf        []byte
	maxInBufLen  int
	singleReadSz int

	wmu            sync.Mutex
	outBufQueue    [][]byte
	outBufTotalLen int
	maxOutBufLen   int
	iovec          []syscall.Iovec
	waitWrite      bool
}

func newBufev(eb *EventBase, conn net.Conn, fd int, file *os.File, maxInBufLen int, singleReadSz int, maxOutBufLen int, cb BufevCallback) *Bufev {
	return &Bufev{
		eb: eb,
		cb: cb,

		conn: conn,
		fd:   fd,
		file: file,

		inBuf:        make([]byte, 0, maxInBufLen),
		maxInBufLen:  maxInBufLen,
		singleReadSz: singleReadSz,

		maxOutBufLen: maxOutBufLen,
	}
}

func (be *Bufev) free(err error) {
	be.mu.Lock()
	if be.freed {
		be.mu.Unlock()
		return
	}
	be.freed = true
	be.changeEventLocked(EV_READ|EV_WRITE, false)
	be.file.Close()
	be.mu.Unlock()

	be.cb.OnFree(be, err)
}

func (be *Bufev) FD() int {
	return be.fd
}

func (be *Bufev) Conn() net.Conn {
	return be.conn
}

func (be *Bufev) SetReadEnable(enable bool) error {
	be.mu.Lock()
	defer be.mu.Unlock()

	if be.freed {
		return ErrBufevFreed
	}

	if be.rEnable == enable {
		return nil
	}
	be.rEnable = enable

	return be.updateReadEventLocked()
}

func (be *Bufev) setReadPause(pause bool) error {
	be.mu.Lock()
	defer be.mu.Unlock()

	if be.freed {
		return ErrBufevFreed
	}

	if be.rPause == pause {
		return nil
	}
	be.rPause = pause

	return be.updateReadEventLocked()
}

func (be *Bufev) updateReadEventLocked() error {
	var enable bool
	if be.rEnable && !be.rPause {
		enable = true
	}
	return be.changeEventLocked(EV_READ, enable)
}

func (be *Bufev) changeEventLocked(event uint32, enable bool) error {
	var old = be.evs
	if enable {
		be.evs |= event
	} else {
		be.evs &= ^event
	}
	if be.evs == old {
		return nil
	}

	if old == 0 {
		if be.evs != 0 {
			//log.Printf("poller add fd=%v, evs=%v", be.fd, be.evs)
			return be.eb.poller.Add(be.fd, be.evs)
		}
	} else {
		if be.evs == 0 {
			//log.Printf("poller del fd=%v", be.fd)
			return be.eb.poller.Del(be.fd)
		} else {
			//log.Printf("poller mod fd=%v, evs=%v", be.fd, be.evs)
			return be.eb.poller.Mod(be.fd, be.evs)
		}
	}
	return nil
}

func (be *Bufev) changeEvent(event uint32, enable bool) error {
	be.mu.Lock()
	defer be.mu.Unlock()

	if be.freed {
		return ErrBufevFreed
	}

	return be.changeEventLocked(event, enable)
}

func (be *Bufev) readOnce() {
	be.mu.Lock()
	if be.freed {
		be.mu.Unlock()
		return
	}
	be.mu.Unlock()

	be.rmu.Lock()

	leftCap := be.maxInBufLen - len(be.inBuf)
	var howmuch int
	if be.singleReadSz > leftCap {
		howmuch = leftCap
	} else {
		howmuch = be.singleReadSz
	}
	if howmuch == 0 {
		be.setReadPause(true)
		be.rmu.Unlock()
		return
	}

	oldLen := len(be.inBuf)
	be.inBuf = be.inBuf[:oldLen+howmuch]

	var err error
	var n int
	for {
		n, err = syscall.Read(be.fd, be.inBuf[oldLen:])
		if err != nil {
			if err == syscall.EINTR {
				continue
			} else if err == syscall.EAGAIN {
				err = nil
				break
			} else {
			}
		} else {
			if n == 0 {
				err = io.EOF
			} else {
				be.inBuf = be.inBuf[:oldLen+n]
			}
		}
		break
	}

	be.rmu.Unlock()

	if err == nil {
		if n != 0 {
			be.cb.OnRead(be)
		}
	} else {
		be.eb.freeBufevWithError(be, err)
		return
	}

	be.rmu.Lock()
	if len(be.inBuf) == be.maxInBufLen {
		be.setReadPause(true)
	}
	be.rmu.Unlock()
}

func (be *Bufev) BufevRead(data []byte, peek bool) (n int, err error) {
	be.rmu.Lock()
	n = copy(data, be.inBuf)
	if !peek {
		oldLen := len(be.inBuf)
		unread := len(be.inBuf) - n
		copy(be.inBuf[:unread], be.inBuf[n:])
		be.inBuf = be.inBuf[:unread]
		if oldLen == be.maxInBufLen && n > 0 {
			err = be.setReadPause(false)
		}
	}
	be.rmu.Unlock()
	return
}

func (be *Bufev) InBufLen() int {
	be.rmu.Lock()
	n := len(be.inBuf)
	be.rmu.Unlock()
	return n
}

func (be *Bufev) writeOnce() {
	be.mu.Lock()
	if be.freed {
		be.mu.Unlock()
		return
	}
	be.mu.Unlock()

	be.wmu.Lock()
	if be.outBufTotalLen == 0 {
		be.waitWrite = false
		be.changeEvent(EV_WRITE, false)
		be.wmu.Unlock()
		return
	}
	err := be.writeOnceImplLocked()
	outBufEmptied := be.outBufTotalLen == 0
	be.wmu.Unlock()

	if err != nil {
		be.eb.freeBufevWithError(be, err)
	} else {
		if outBufEmptied {
			be.cb.OnWrite(be)
		}
	}
}

func (be *Bufev) writeOnceImplLocked() (err error) {
	be.iovec = be.iovec[:0]
	for _, bs := range be.outBufQueue {
		be.iovec = append(be.iovec, syscall.Iovec{Base: &bs[0], Len: uint64(len(bs))})
	}

	for {
		rawwn, _, en := syscall.Syscall(syscall.SYS_WRITEV, uintptr(be.fd), uintptr(unsafe.Pointer(&be.iovec[0])), uintptr(len(be.iovec)))
		if en != 0 {
			if en == syscall.EINTR {
				continue
			} else if en == syscall.EAGAIN {
			} else {
				err = en
			}
		} else {
			wn := int(rawwn)
			var j int
			for i, bs := range be.outBufQueue {
				if wn >= len(bs) {
					be.outBufQueue[i] = nil
					wn -= len(bs)
					be.outBufTotalLen -= len(bs)
				} else {
					be.outBufQueue[i] = nil
					be.outBufQueue[j] = bs[wn:]
					j++
					be.outBufTotalLen -= wn
					wn = 0
				}
			}
			be.outBufQueue = be.outBufQueue[:j]
		}
		if err == nil {
			if be.outBufTotalLen > 0 {
				if !be.waitWrite {
					err = be.changeEvent(EV_WRITE, true)
					be.waitWrite = true
				}
			} else {
				if be.waitWrite {
					be.waitWrite = false
					err = be.changeEvent(EV_WRITE, false)
				}
			}
		}
		break
	}
	return
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

	be.outBufQueue = append(be.outBufQueue, data)

	if !be.waitWrite {
		err = be.writeOnceImplLocked()
	}

	var outBufEmptied bool
	if err == nil {
		if be.outBufTotalLen > be.maxOutBufLen {
			clipBytes := be.outBufTotalLen - be.maxOutBufLen
			n = len(data) - clipBytes

			be.outBufTotalLen -= clipBytes

			for i := len(be.outBufQueue) - 1; i >= 0; i-- {
				if len(be.outBufQueue[i]) <= clipBytes {
					clipBytes -= len(be.outBufQueue[i])
					be.outBufQueue[i] = nil
					be.outBufQueue = be.outBufQueue[:i]
				} else {
					be.outBufQueue[i] = be.outBufQueue[i][:len(be.outBufQueue[i])-clipBytes]
					break
				}
			}
			be.wmu.Unlock()
			return n, ErrWriteBufferLimitReached
		} else if be.outBufTotalLen == 0 {
			outBufEmptied = true
		}
		n = len(data)
	}
	be.wmu.Unlock()

	if err != nil {
		be.eb.freeBufevWithError(be, err)
	} else {
		if outBufEmptied {
			be.cb.OnWrite(be)
		}
	}
	return
}

type EventBase struct {
	mu       sync.Mutex
	fd2Bufev map[int]*Bufev
	poller   *poller
	closed   bool
}

func NewEventBase() (*EventBase, error) {
	eb := &EventBase{
		fd2Bufev: make(map[int]*Bufev),
	}
	var err error
	eb.poller, err = NewPoller()
	if err != nil {
		return nil, err
	}
	go eb.loop()
	return eb, err
}

func (eb *EventBase) NewBufev(conn net.Conn, maxInBufLen, singleReadSz int, maxOutBufLen int, cb BufevCallback) (*Bufev, error) {
	f, ok := conn.(interface {
		File() (*os.File, error)
	})
	if !ok {
		return nil, ErrUnsupported
	}
	file, err := f.File()
	if err != nil {
		return nil, err
	}
	fd := int(file.Fd())

	eb.mu.Lock()
	defer eb.mu.Unlock()

	if eb.closed {
		return nil, ErrEvtBaseClosed
	}

	if eb.fd2Bufev[fd] != nil {
		return nil, ErrConnAlreadyBound
	}

	if err = syscall.SetNonblock(fd, true); err != nil {
		return nil, err
	}
	if singleReadSz > maxInBufLen {
		singleReadSz = maxInBufLen
	}
	be := newBufev(eb, conn, fd, file, maxInBufLen, singleReadSz, maxOutBufLen, cb)
	eb.fd2Bufev[fd] = be
	return be, nil
}

func (eb *EventBase) FreeBufev(be *Bufev) {
	eb.freeBufevWithError(be, nil)
}

func (eb *EventBase) freeBufevWithError(be *Bufev, err error) {
	be.free(err)
	eb.mu.Lock()
	delete(eb.fd2Bufev, be.fd)
	eb.mu.Unlock()
}

func (eb *EventBase) Close() {
	bes := make(map[int]*Bufev)

	eb.mu.Lock()
	if eb.closed {
		eb.mu.Unlock()
		return
	}
	eb.closed = true
	eb.poller.Close()
	for fd, be := range eb.fd2Bufev {
		bes[fd] = be
	}
	eb.mu.Unlock()

	for _, be := range bes {
		eb.freeBufevWithError(be, ErrEvtBaseClosed)
	}
}

func (eb *EventBase) loop() {
	defer eb.Close()
	var rbevs, wbevs []*Bufev
	for {
		ens, err := eb.poller.Wait()
		if err != nil {
			return
		}

		rbevs, wbevs = rbevs[:0], wbevs[:0]
		eb.mu.Lock()
		for _, e := range ens {
			bev := eb.fd2Bufev[e.fd]
			if bev == nil {
				continue
			}
			if e.evs&EV_READ != 0 {
				rbevs = append(rbevs, bev)
			} else if e.evs&EV_WRITE != 0 {
				wbevs = append(wbevs, bev)
			}
		}
		eb.mu.Unlock()

		for i, rbev := range rbevs {
			rbevs[i] = nil
			rbev.readOnce()
		}

		for i, wbev := range wbevs {
			wbevs[i] = nil
			wbev.writeOnce()
		}
	}
}
