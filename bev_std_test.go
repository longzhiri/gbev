// +build !linux,!darwin,!netbsd,!freebsd,!openbsd,!dragonfly std

package gbev

import (
	"log"
	"sync"
)

type echoClientHandler struct {
	mu      sync.Mutex
	rsignal chan struct{}
	wsignal chan struct{}

	data          []byte
	unwrittenData []byte
}

func (ch *echoClientHandler) OnRead(be *Bufev) {
	ch.mu.Lock()
	if ch.rsignal == nil {
		ch.rsignal = make(chan struct{}, 1)
		ch.wsignal = make(chan struct{})
		go func() {
			for {
				select {
				case _, ok := <-ch.rsignal:
					if !ok {
						return
					}
					for {
						n, err := be.BufevRead(ch.data, false)
						if err != nil {
							log.Printf("BufevRead error: %v", err)
						} else {
							if n == 0 {
								break
							}
							ch.unwrittenData = ch.data[:n]
							for len(ch.unwrittenData) > 0 {
								wn, werr := be.BufevWrite(ch.unwrittenData)
								if werr != nil {
									if werr == ErrWriteBufferLimitReached {
										ch.unwrittenData = ch.unwrittenData[wn:]
									} else {
										panic(werr)
									}
								} else {
									ch.unwrittenData = nil
								}
								_, ok2 := <-ch.wsignal
								if !ok2 {
									return
								}
							}
						}
					}
				}
			}
		}()
	}
	select {
	case ch.rsignal <- struct{}{}:
	default:
	}
	ch.mu.Unlock()
}

func (ch *echoClientHandler) OnWrite(be *Bufev) {
	ch.mu.Lock()
	ch.wsignal <- struct{}{}
	ch.mu.Unlock()
}

func (ch *echoClientHandler) OnFree(be *Bufev, err error) {
	//	log.Printf("bufev=%v freed, err=%v", be.Conn().RemoteAddr().String(), err)
	ch.mu.Lock()
	if ch.rsignal != nil {
		close(ch.rsignal)
		close(ch.wsignal)
	}
	ch.mu.Unlock()
}
