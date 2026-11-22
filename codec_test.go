//go:build linux
// +build linux

package uringnet

import (
	"bufio"
	"bytes"
	"encoding/binary"
	"errors"
	"io"
	mrand "math/rand"
	"net"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestLengthFieldFramer(t *testing.T) {
	for _, size := range []int{1, 2, 4, 8} {
		for _, order := range []binary.ByteOrder{binary.BigEndian, binary.LittleEndian} {
			f := NewLengthField(size, order, 200)
			frame := f.AppendFrame([]byte("x"), []byte("hello"))[1:] // 追加到已有数据之后
			if n, err := f.Frame(frame); n != size+5 || err != nil {
				t.Fatalf("size %d %v: Frame = %d, %v", size, order, n, err)
			}
			if got := f.Payload(frame); string(got) != "hello" {
				t.Fatalf("payload %q", got)
			}
			for i := 0; i < len(frame); i++ {
				if n, err := f.Frame(frame[:i]); n != 0 || err != nil {
					t.Fatalf("size %d: partial frame of %d bytes: %d, %v", size, i, n, err)
				}
			}
			if n, _ := f.Frame(append(frame, 'y')); n != size+5 {
				t.Fatalf("frame followed by more data: %d", n)
			}
		}
	}
	f := NewLengthField(2, binary.BigEndian, 100)
	if _, err := f.Frame([]byte{0, 101}); !errors.Is(err, ErrFrameTooLarge) {
		t.Fatalf("oversized frame: %v", err)
	}
}

func TestDelimiterFramer(t *testing.T) {
	d := NewDelimiter([]byte("\r\n"), 10)
	for _, tc := range []struct {
		in   string
		n    int
		fail bool
	}{
		{"", 0, false},
		{"abc", 0, false},
		{"abc\r", 0, false},
		{"abc\r\n", 5, false},
		{"\r\nabc", 2, false},
		{"abcdefgh\r\n", 10, false},
		{"abcdefghi\r\n", 0, true}, // 分隔符不在前 10 字节内
		{"abcdefghij", 0, true},
	} {
		n, err := d.Frame([]byte(tc.in))
		if n != tc.n || (err != nil) != tc.fail {
			t.Fatalf("Frame(%q) = %d, %v", tc.in, n, err)
		}
	}
	if got := d.Payload([]byte("abc\r\n")); string(got) != "abc" {
		t.Fatalf("payload %q", got)
	}
	if got := d.AppendFrame(nil, []byte("hi")); string(got) != "hi\r\n" {
		t.Fatalf("AppendFrame %q", got)
	}
}

func TestFixedLengthFramer(t *testing.T) {
	f := FixedLength(4)
	if n, _ := f.Frame([]byte("abc")); n != 0 {
		t.Fatal(n)
	}
	if n, _ := f.Frame([]byte("abcdef")); n != 4 {
		t.Fatal(n)
	}
}

// frameEcho 把每一帧原样写回，并统计收到的帧
type frameEcho struct {
	BuiltinHandler
	frames atomic.Int64
	mu     sync.Mutex
	errs   []error
}

func (h *frameEcho) OnMessage(c *Conn, msg []byte) Action {
	h.frames.Add(1)
	c.Write(msg)
	return None
}

func (h *frameEcho) OnClose(c *Conn, err error) {
	h.mu.Lock()
	h.errs = append(h.errs, err)
	h.mu.Unlock()
}

// 长度前缀帧在随机分包下：帧数与内容都正确（帧完整到达时原地写回，跨包时从缓存写回）
func TestFramedLengthField(t *testing.T) {
	lf := NewLengthField(2, binary.BigEndian, 4096)
	h := &frameEcho{}
	_, addr := serve(t, Framed(lf, h), WithRings(1), WithBuffers(16, 2048))

	rng := mrand.New(mrand.NewSource(1))
	var stream []byte
	const frames = 2000
	for i := 0; i < frames; i++ {
		payload := make([]byte, rng.Intn(4097))
		rng.Read(payload)
		stream = lf.AppendFrame(stream, payload)
	}
	if err := echoChunked(addr, stream, 7); err != nil {
		t.Fatal(err)
	}
	if got := h.frames.Load(); got != frames {
		t.Fatalf("OnMessage called %d times, want %d", got, frames)
	}
}

// 按行处理的协议：每行转成大写回复；超长的行关闭连接
type upperLines struct {
	BuiltinHandler
	d    *Delimiter
	errs chan error
}

func (h *upperLines) OnMessage(c *Conn, msg []byte) Action {
	line := h.d.Payload(msg)
	if string(line) == "quit" {
		return Close
	}
	c.Write(h.d.AppendFrame(nil, bytes.ToUpper(line)))
	return None
}

func (h *upperLines) OnClose(c *Conn, err error) { h.errs <- err }

func TestFramedDelimiter(t *testing.T) {
	d := NewDelimiter([]byte("\n"), 64)
	h := &upperLines{d: d, errs: make(chan error, 4)}
	_, addr := serve(t, Framed(d, h), WithRings(1))

	c, err := net.Dial("tcp4", addr)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	c.SetDeadline(time.Now().Add(5 * time.Second))
	// 一次写入多行，并把一行拆成两次写入
	c.Write([]byte("hello\nworld\nsp"))
	time.Sleep(20 * time.Millisecond)
	c.Write([]byte("lit\nquit\nignored\n"))
	got, err := io.ReadAll(bufio.NewReader(c))
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "HELLO\nWORLD\nSPLIT\n" {
		t.Fatalf("got %q", got)
	}
	if err := <-h.errs; err != nil {
		t.Fatalf("OnClose after quit: %v", err)
	}

	long, err := net.Dial("tcp4", addr)
	if err != nil {
		t.Fatal(err)
	}
	defer long.Close()
	long.Write([]byte(strings.Repeat("x", 100)))
	select {
	case err := <-h.errs:
		if !errors.Is(err, ErrFrameTooLarge) {
			t.Fatalf("OnClose for an oversized line: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("connection with an oversized line was not closed")
	}
}
