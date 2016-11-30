package multiplex

import (
	"bufio"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net"
	"sync"
	"time"

	mpool "github.com/jbenet/go-msgio/mpool"
)

var MaxMessageSize = 1 << 20

const (
	NewStream = iota
	Receiver
	Initiator
	Unknown
	Close
)

type Multiplex struct {
	con       net.Conn
	buf       *bufio.Reader
	nextID    uint64
	closed    chan struct{}
	initiator bool

	wrLock sync.Mutex

	nstreams chan *Stream
	errs     chan error

	hdrBuf []byte

	channels map[uint64]*Stream
	chLock   sync.Mutex
}

func NewMultiplex(con net.Conn, initiator bool) *Multiplex {
	mp := &Multiplex{
		con:       con,
		initiator: initiator,
		buf:       bufio.NewReader(con),
		channels:  make(map[uint64]*Stream),
		closed:    make(chan struct{}),
		nstreams:  make(chan *Stream, 16),
		errs:      make(chan error),
		hdrBuf:    make([]byte, 20),
	}

	go mp.handleIncoming()

	return mp
}

func (mp *Multiplex) newStream(id uint64, name string, initiator bool) *Stream {
	var hfn uint64
	if initiator {
		hfn = 2
	} else {
		hfn = 1
	}
	return &Stream{
		id:     id,
		name:   name,
		header: (id << 3) | hfn,
		dataIn: make(chan []byte, 8),
		clCh:   make(chan struct{}),
		mp:     mp,
	}
}

func (m *Multiplex) Accept() (*Stream, error) {
	select {
	case s, ok := <-m.nstreams:
		if !ok {
			return nil, errors.New("multiplex closed")
		}
		return s, nil
	case err := <-m.errs:
		return nil, err
	case <-m.closed:
		return nil, errors.New("multiplex closed")
	}
}

func (mp *Multiplex) Close() error {
	if mp.IsClosed() {
		return nil
	}
	close(mp.closed)
	mp.chLock.Lock()
	defer mp.chLock.Unlock()
	for _, s := range mp.channels {
		err := s.Close()
		if err != nil {
			return err
		}
	}
	return nil
}

func (mp *Multiplex) IsClosed() bool {
	select {
	case <-mp.closed:
		return true
	default:
		return false
	}
}

func (mp *Multiplex) sendMsg(header uint64, data []byte, dl time.Time) error {
	mp.wrLock.Lock()
	defer mp.wrLock.Unlock()
	if !dl.IsZero() {
		if err := mp.con.SetWriteDeadline(dl); err != nil {
			return err
		}
	}
	n := binary.PutUvarint(mp.hdrBuf, header)
	n += binary.PutUvarint(mp.hdrBuf[n:], uint64(len(data)))
	_, err := mp.con.Write(mp.hdrBuf[:n])
	if err != nil {
		return err
	}

	if len(data) != 0 {
		_, err = mp.con.Write(data)
		if err != nil {
			return err
		}
	}
	if !dl.IsZero() {
		if err := mp.con.SetWriteDeadline(time.Time{}); err != nil {
			return err
		}
	}

	return nil
}

func (mp *Multiplex) nextChanID() (out uint64) {
	if mp.initiator {
		out = mp.nextID + 1
	} else {
		out = mp.nextID
	}
	mp.nextID += 2
	return
}

func (mp *Multiplex) NewStream() (*Stream, error) {
	return mp.NewNamedStream("")
}

func (mp *Multiplex) NewNamedStream(name string) (*Stream, error) {
	mp.chLock.Lock()
	sid := mp.nextChanID()
	header := (sid << 3) | NewStream

	if name == "" {
		name = fmt.Sprint(sid)
	}
	s := mp.newStream(sid, name, true)
	mp.channels[sid] = s
	mp.chLock.Unlock()

	err := mp.sendMsg(header, []byte(name), time.Time{})
	if err != nil {
		return nil, err
	}

	return s, nil
}

func (mp *Multiplex) sendErr(err error) {
	select {
	case mp.errs <- err:
	case <-mp.closed:
	}
}

func (mp *Multiplex) handleIncoming() {
	defer mp.Close()
	for {
		ch, tag, err := mp.readNextHeader()
		if err != nil {
			mp.sendErr(err)
			return
		}

		b, err := mp.readNext()
		if err != nil {
			mp.sendErr(err)
			return
		}

		mp.chLock.Lock()
		msch, ok := mp.channels[ch]
		mp.chLock.Unlock()
		if !ok {
			var name string
			if tag == NewStream {
				name = string(b)
			}
			msch = mp.newStream(ch, name, false)
			mp.channels[ch] = msch
			select {
			case mp.nstreams <- msch:
			case <-mp.closed:
				return
			}
			if tag == NewStream {
				continue
			}
		}

		if tag == Close {
			close(msch.dataIn)
			mp.chLock.Lock()
			delete(mp.channels, ch)
			mp.chLock.Unlock()
			continue
		}

		msch.receive(b)
	}
}

func (mp *Multiplex) readNextHeader() (uint64, uint64, error) {
	h, err := binary.ReadUvarint(mp.buf)
	if err != nil {
		return 0, 0, err
	}

	// get channel ID
	ch := h >> 3

	rem := h & 7

	return ch, rem, nil
}

func (mp *Multiplex) readNext() ([]byte, error) {
	// get length
	l, err := binary.ReadUvarint(mp.buf)
	if err != nil {
		return nil, err
	}

	if l > uint64(MaxMessageSize) {
		return nil, fmt.Errorf("message size too large!")
	}

	if l == 0 {
		return nil, nil
	}

	buf := mpool.ByteSlicePool.Get(uint32(l)).([]byte)[:l]
	n, err := io.ReadFull(mp.buf, buf)
	if err != nil {
		return nil, err
	}

	return buf[:n], nil
}
