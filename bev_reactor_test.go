// +build linux darwin netbsd freebsd openbsd dragonfly
// +build !std

package gbev

import "log"

type echoClientHandler struct {
	data          []byte
	writing       bool
	unwrittenData []byte
}

func (ch *echoClientHandler) OnRead(be *Bufev) {
	if ch.writing {
		return
	}
	n, err := be.BufevRead(ch.data, false)
	if err != nil {
		log.Printf("BufevRead error: %v", err)
	} else {
		if n == 0 {
			return
		}
		ch.writing = true
		wn, werr := be.BufevWrite(ch.data[:n])
		if werr != nil {
			if werr == ErrWriteBufferLimitReached {
				ch.unwrittenData = ch.data[wn:n]
			} else {
				panic(werr)
			}
		}
	}
}

func (ch *echoClientHandler) OnWrite(be *Bufev) {
	if len(ch.unwrittenData) > 0 {
		unwrittenData := ch.unwrittenData
		ch.unwrittenData = nil
		wn, werr := be.BufevWrite(unwrittenData)
		if werr != nil {
			if werr == ErrWriteBufferLimitReached {
				ch.unwrittenData = unwrittenData[wn:]
			} else {
				panic(werr)
			}
			return
		}
	}
	ch.writing = false
	ch.OnRead(be)
}

func (ch *echoClientHandler) OnFree(be *Bufev, err error) {
	//	log.Printf("bufev=%v freed, err=%v", be.Conn().RemoteAddr().String(), err)
}
