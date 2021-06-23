package gbev

import (
	"io"
	"log"
	"math"
	"math/rand"
	"net"
	_ "net/http/pprof"
	"seasun/sync"
	"testing"
	"time"
)

/*
type echoClientHandler struct {
	unwrittenData []byte
}

func (ch *echoClientHandler) OnRead(be *Bufev) {
	if ch.unwrittenData != nil {
		return
	}
	data := make([]byte, be.InBufLen())
	n, err := be.BufevRead(data, false)
	if err != nil {
		log.Printf("BufevRead error: %v", err)
	} else {
		wn, werr := be.BufevWrite(data[:n])
		if werr != nil {
			if werr != ErrWriteBufferLimitReached {
				panic(werr)
			} else {
				ch.unwrittenData = data[wn:n]
			}
		}
	}
}

func (ch *echoClientHandler) OnWrite(be *Bufev) {
	unwrittenData := ch.unwrittenData
	ch.unwrittenData = nil
	wn, werr := be.BufevWrite(unwrittenData)
	if werr != nil {
		if werr != ErrWriteBufferLimitReached {
			panic(werr)
		} else {
			ch.unwrittenData = unwrittenData[wn:]
		}
	} else {
		ch.OnRead(be)
	}
}
*/

func setupEchoServer(inBufLen int, singleReadSz int, outBufLen int) (*EventBase, net.Listener) {
	tcpaddr, _ := net.ResolveTCPAddr("tcp", "localhost:0")
	l, err := net.ListenTCP("tcp", tcpaddr)
	if err != nil {
		panic(err)
	}
	eb, err := NewEventBase()
	if err != nil {
		panic(err)
	}

	go func() {
		for {
			conn, err := l.Accept()
			if err != nil {
				log.Printf("accept error:%v", err)
				return
			}
			h := new(echoClientHandler)
			h.data = make([]byte, singleReadSz)
			be, err := eb.NewBufev(conn, inBufLen, singleReadSz, outBufLen, h)
			if err != nil {
				panic(err)
			}
			be.SetReadEnable(true)
		}
	}()

	/*
		go func() {
			log.Println(http.ListenAndServe("0.0.0.0:12307", nil))
		}()
	*/
	return eb, l
}

func closeEchoServer(eb *EventBase, l net.Listener) {
	eb.Close()
	l.Close()
}

type echoServerHandler struct {
	singleWriteSzRange [2]int

	leftDataWritten int
	leftDataRead    int
	wg              *sync.WaitGroup

	inBuf  []byte
	outBuf []byte

	be  *Bufev
	i   int
	shc chan *echoServerHandler
}

func (sh *echoServerHandler) OnRead(be *Bufev) {
	n, err := be.BufevRead(sh.inBuf[:cap(sh.inBuf)], false)
	if err != nil {
		log.Printf("BufevRead error: %v", err)
		sh.wg.Done()
		return
	} else {
		sh.leftDataRead -= n
		if sh.leftDataRead <= 0 {
			//log.Printf("read done i=%v", sh.i)
			sh.wg.Done()
		}
	}
}

func (sh *echoServerHandler) OnWrite(be *Bufev) {
	sh.shc <- sh
}

func (sh *echoServerHandler) OnFree(be *Bufev, err error) {
	//	log.Printf("bufev=%v freed, err=%v", be.Conn().RemoteAddr().String(), err)
}

func (sh *echoServerHandler) writeEchoOnce(be *Bufev) {
	if sh.leftDataWritten == 0 {
		return
	}

	n := rand.Intn(sh.singleWriteSzRange[1]-sh.singleWriteSzRange[0]+1) + sh.singleWriteSzRange[0]
	if n > sh.leftDataWritten {
		n = sh.leftDataWritten
	}
	sh.leftDataWritten -= n
	_, werr := be.BufevWrite(sh.outBuf[:n])
	if werr != nil {
		panic(werr)
	}
}

func setupEchoBevClient(eb *EventBase, l net.Listener, num int, singleWriteSzRange [2]int, writeTotalSz int) []*echoServerHandler {
	var hs []*echoServerHandler
	var shc chan *echoServerHandler
	shc = make(chan *echoServerHandler, 1024)
	go func() {
		for {
			select {
			case sh := <-shc:
				if sh == nil {
					return
				} else {
					sh.writeEchoOnce(sh.be)
				}
			}
		}
	}()
	for i := 0; i < num; i++ {
		conn, err := net.Dial("tcp", l.Addr().String())
		if err != nil {
			panic(err)
		}
		h := &echoServerHandler{
			singleWriteSzRange: singleWriteSzRange,
			leftDataWritten:    writeTotalSz,
			leftDataRead:       writeTotalSz,
			inBuf:              make([]byte, singleWriteSzRange[1]),
			outBuf:             make([]byte, singleWriteSzRange[1]),
			i:                  i,
			shc:                shc,
		}
		be, err := eb.NewBufev(conn, singleWriteSzRange[1], singleWriteSzRange[1], singleWriteSzRange[1], h)
		if err != nil {
			panic(err)
		}
		h.be = be
		hs = append(hs, h)
	}

	return hs
}

func setupEchoParallelClient(l net.Listener, num int, singleWriteSzRange [2]int, writeTotalSz int, check bool, disconnectRate int) {
	outBuf := make([]byte, singleWriteSzRange[1])
	for i := range outBuf {
		outBuf[i] = byte(rand.Intn(math.MaxInt8))
	}

	var wg sync.WaitGroup
	wg.Add(num)

	for i := 0; i < num; i++ {
		go func(i int) {
			defer wg.Done()

			conn, err := net.Dial("tcp", l.Addr().String())
			if err != nil {
				panic(err)
			}

			inBuf := make([]byte, singleWriteSzRange[1])

			leftn := writeTotalSz
			for leftn > 0 {
				n := rand.Intn(singleWriteSzRange[1]-singleWriteSzRange[0]+1) + singleWriteSzRange[0]
				if n > leftn {
					n = leftn
				}
				leftn -= n

				_, err = conn.Write(outBuf[:n])
				if err != nil {
					conn.Close()
					log.Printf("echo client write error:%v", err)
					return
				}

				_, err = io.ReadFull(conn, inBuf[:n])
				if err != nil {
					conn.Close()
					log.Printf("echo client read full error:%v", err)
					return
				}
				if check {
					for j := 0; j < n; j++ {
						if outBuf[j] != inBuf[j] {
							panic("unexpected")
						}
					}
				}
				if disconnectRate > 0 && rand.Intn(100) < disconnectRate {
					conn.Close()
					break
				}

			}
			//log.Printf("client=%v done", i)

		}(i)
	}
	wg.Wait()
}

func TestEchoTiny(t *testing.T) {
	eb, l := setupEchoServer(16, 8, 16)
	setupEchoParallelClient(l, 100, [2]int{1, 2}, 1024*100, true, 0)
	closeEchoServer(eb, l)
}

func TestEchoHuge(t *testing.T) {
	eb, l := setupEchoServer(1024*1024*128, 1024*1024*64, 1024*1024*200)
	setupEchoParallelClient(l, 100, [2]int{1024 * 1024 * 64, 1024 * 1024 * 512}, 1024*1024*1024*30, true, 0)
	closeEchoServer(eb, l)
}

func TestEchoMedium(t *testing.T) {
	eb, l := setupEchoServer(1024*512, 1024*16, 1024*512)
	setupEchoParallelClient(l, 100, [2]int{4, 1024 * 16}, 1024*1024*1024, true, 0)
	closeEchoServer(eb, l)
}

func TestClientDisconnect(t *testing.T) {
	eb, l := setupEchoServer(1024*512, 1024*16, 1024*512)
	setupEchoParallelClient(l, 100, [2]int{4, 1024 * 16}, 1024*1024*100, true, 10)
	closeEchoServer(eb, l)
}

func TestServerDisconnect(t *testing.T) {
	eb, l := setupEchoServer(1024*512, 1024*16, 1024*512)
	go func() {
		time.Sleep(time.Duration(rand.Int63n(10)+1) * time.Second)
		closeEchoServer(eb, l)
	}()
	setupEchoParallelClient(l, 100, [2]int{4, 1024 * 16}, 1024*1024*100, true, 10)
}

func Benchmark128Echo(b *testing.B) {
	benchmarkFixedSizeBytesEcho(b, 1, 128)
}

func Benchmark1kEcho(b *testing.B) {
	benchmarkFixedSizeBytesEcho(b, 1, 1024)
}

func Benchmark4kEcho(b *testing.B) {
	benchmarkFixedSizeBytesEcho(b, 1, 1024*4)
}

func Benchmark64kEcho(b *testing.B) {
	benchmarkFixedSizeBytesEcho(b, 1, 1024*64)
}

func Benchmark128kEcho(b *testing.B) {
	benchmarkFixedSizeBytesEcho(b, 1, 1024*128)
}

func Benchmark128EchoParallel(b *testing.B) {
	benchmarkFixedSizeBytesEcho(b, 128, 128)
}

func Benchmark1kEchoParallel(b *testing.B) {
	benchmarkFixedSizeBytesEcho(b, 128, 1024)
}

func Benchmark4kEchoParallel(b *testing.B) {
	benchmarkFixedSizeBytesEcho(b, 128, 1024*4)
}

func Benchmark64kEchoParallel(b *testing.B) {
	benchmarkFixedSizeBytesEcho(b, 128, 1024*64)
}

func Benchmark128kEchoParallel(b *testing.B) {
	benchmarkFixedSizeBytesEcho(b, 128, 1024*128)
}

func benchmarkFixedSizeBytesEcho(b *testing.B, clientNum int, fixedSize int) {
	benchmarkEcho(b, clientNum, [2]int{fixedSize, fixedSize}, fixedSize*b.N)
}

func benchmarkEcho(b *testing.B, clientNum int, singleWriteSzRange [2]int, writeTotalSzPerClient int) {
	eb, l := setupEchoServer(singleWriteSzRange[1]*20, singleWriteSzRange[1]*3, singleWriteSzRange[1]*50)

	clientEB, err := NewEventBase()
	if err != nil {
		panic(err)
	}

	hs := setupEchoBevClient(clientEB, l, clientNum, singleWriteSzRange, writeTotalSzPerClient)
	b.SetBytes(int64(clientNum * writeTotalSzPerClient / b.N))
	b.ResetTimer()

	var wg sync.WaitGroup
	for _, h := range hs {
		wg.Add(1)
		h.wg = &wg
		h.writeEchoOnce(h.be)
		h.be.SetReadEnable(true)
	}
	wg.Wait()

	if hs[0].shc != nil {
		hs[0].shc <- nil
		close(hs[0].shc)
	}
	clientEB.Close()

	closeEchoServer(eb, l)

}

func BenchmarkRandomBytesEcho(b *testing.B) {
	benchmarkEcho(b, 100, [2]int{4, 1024 * 16}, 1024*1024*100*b.N)
}
