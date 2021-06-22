// +build linux

package gbev

import (
	"math"
	"sync"
	"syscall"
	"unsafe"
)

const (
	_EPOLLET      = 0x80000000
	_EFD_NONBLOCK = 0x800
)

const (
	maxEvents = 4096
)

const (
	closing = iota + 1
	closed
)

type poller struct {
	pfd int
	efd int

	events []syscall.EpollEvent
	pes    []pollEvent

	mu         sync.Mutex
	closeState int8
}

func NewPoller() (*poller, error) {
	fd, err := syscall.EpollCreate1(syscall.EPOLL_CLOEXEC)
	if err != nil {
		return nil, err
	}
	r0, _, e0 := syscall.Syscall(syscall.SYS_EVENTFD2, 0, _EFD_NONBLOCK, 0)
	if e0 != 0 {
		syscall.Close(fd)
		return nil, err
	}

	if err := syscall.EpollCtl(fd, syscall.EPOLL_CTL_ADD, int(r0),
		&syscall.EpollEvent{Fd: int32(r0),
			Events: syscall.EPOLLIN | _EPOLLET,
		},
	); err != nil {
		syscall.Close(fd)
		syscall.Close(int(r0))
		return nil, err
	}

	p := new(poller)
	p.pfd = fd
	p.efd = int(r0)
	p.events = make([]syscall.EpollEvent, maxEvents)
	return p, nil
}

func evs2EpollEvents(evs uint32) uint32 {
	var ees uint32
	if evs&EV_READ != 0 {
		ees |= syscall.EPOLLIN | syscall.EPOLLRDHUP
	}
	if evs&EV_WRITE != 0 {
		ees |= syscall.EPOLLOUT
	}
	return ees
}

func (p *poller) Add(fd int, events uint32) (err error) {
	p.mu.Lock()
	if p.closeState == 0 {
		err = syscall.EpollCtl(p.pfd, syscall.EPOLL_CTL_ADD, fd, &syscall.EpollEvent{Fd: int32(fd), Events: evs2EpollEvents(events)})
	} else {
		err = ErrPollerClosed
	}
	p.mu.Unlock()
	return
}

func (p *poller) Del(fd int) (err error) {
	p.mu.Lock()
	if p.closeState == 0 {
		err = syscall.EpollCtl(p.pfd, syscall.EPOLL_CTL_DEL, fd, nil)
	} else {
		err = ErrPollerClosed
	}
	p.mu.Unlock()
	return
}

func (p *poller) Mod(fd int, events uint32) (err error) {
	p.mu.Lock()
	if p.closeState == 0 {
		err = syscall.EpollCtl(p.pfd, syscall.EPOLL_CTL_MOD, fd, &syscall.EpollEvent{Fd: int32(fd), Events: evs2EpollEvents(events)})
	} else {
		err = ErrPollerClosed
	}
	p.mu.Unlock()
	return
}

func (p *poller) Wait() ([]pollEvent, error) {
	p.mu.Lock()
	if p.closeState == closed {
		p.mu.Unlock()
		return nil, ErrPollerClosed
	}
	p.mu.Unlock()

	for {
		n, err := syscall.EpollWait(p.pfd, p.events, -1)
		if err != nil {
			if err == syscall.EINTR {
				continue
			}
			p.closeExec()
			return nil, err
		} else {
			p.pes = p.pes[:0]
			for i := 0; i < n; i++ {
				if int(p.events[i].Fd) == p.efd {
					var x uint64
					syscall.Read(p.efd, (*(*[8]byte)(unsafe.Pointer(&x)))[:])
					cpuid := int32(x)
					if cpuid == math.MaxInt32 {
						p.closeExec()
						return nil, ErrPollerClosed
					} else {
						setAffinity(cpuid)
						continue
					}
				}
				what := p.events[i].Events
				var ev uint32
				if what&(syscall.EPOLLERR|syscall.EPOLLHUP) != 0 {
					ev = EV_READ | EV_WRITE
				} else if what&(syscall.EPOLLIN|syscall.EPOLLRDHUP) != 0 {
					ev = EV_READ
				} else if what&(syscall.EPOLLOUT) != 0 {
					ev = EV_WRITE
				}
				p.pes = append(p.pes, pollEvent{fd: int(p.events[i].Fd), evs: ev})
			}
			return p.pes, nil
		}
	}
}

func (p *poller) Close() (err error) {
	p.mu.Lock()
	if p.closeState == 0 {
		p.closeState = closing
		err = p.signalLocked(math.MaxInt32)
	} else {
		err = ErrPollerClosed
	}
	p.mu.Unlock()
	return
}

func (p *poller) closeExec() {
	p.mu.Lock()
	syscall.Close(p.pfd)
	syscall.Close(p.efd)
	p.closeState = closed
	p.mu.Unlock()
}

func (p *poller) signalLocked(cpuid int32) error {
	x := uint64(cpuid)
	_, err := syscall.Write(p.efd, (*(*[8]byte)(unsafe.Pointer(&x)))[:])
	return err
}
