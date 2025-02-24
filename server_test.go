package styx

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"aqwari.net/net/styx/internal/netutil"
	"aqwari.net/net/styx/internal/styxfile"
	"aqwari.net/net/styx/styxproto"
)

const (
	maxuint32 = 1<<32 - 1
	maxuint16 = 1<<16 - 1
)

type testLogger struct {
	*testing.T
}

func (t testLogger) Printf(format string, args ...interface{}) {
	t.Logf(format, args...)
}

type testServer struct {
	callback func(req, rsp styxproto.Msg)
	handler  Handler
	test     *testing.T
}

func openfile(filename string) (*os.File, func()) {
	file, err := os.Open(filename)
	if err != nil {
		panic(err)
	}
	return file, func() { file.Close() }
}

type slowFile struct {
	blockme chan struct{}
	closeme chan struct{}
	mu      sync.Mutex
	name    string
}

func (f *slowFile) Read(p []byte) (int, error) {
	select {
	case <-f.blockme:
		return len(p), nil
	case <-f.closeme:
		return 0, errors.New("closed")
	}
}

// os.FileInfo
func (f *slowFile) Mode() os.FileMode  { return 0 }
func (f *slowFile) IsDir() bool        { return false }
func (f *slowFile) Name() string       { return f.name }
func (f *slowFile) Sys() interface{}   { return nil }
func (f *slowFile) Size() int64        { return 100000 }
func (f *slowFile) ModTime() time.Time { return time.Time{} }
func (f *slowFile) Close() error {
	f.mu.Lock()
	select {
	case <-f.closeme:
	default:
		close(f.closeme)
	}
	f.mu.Unlock()
	return nil
}

// A file system with nothing in it
type emptyFS int

func (emptyFS) Serve9P(s *Session) {
	for s.Next() {
		switch req := s.Request().(type) {
		case Tstat:
			if req.Path() == "/" {
				req.Rstat(emptyDir{emptyStatDir(req.Path())}, nil)
			}
		case Topen:
			req.Ropen(emptyDir{emptyStatDir(req.Path())}, nil)
		}
	}
}

type emptyStatFile string

// fs.FileInfo
func (s emptyStatFile) Mode() os.FileMode  { return 0222 }
func (s emptyStatFile) IsDir() bool        { return s.Mode().IsDir() }
func (s emptyStatFile) Name() string       { return string(s) }
func (s emptyStatFile) Sys() interface{}   { return nil }
func (s emptyStatFile) Size() int64        { return 0 }
func (s emptyStatFile) ModTime() time.Time { return time.Time{} }

type emptyStatDir string

// fs.FileInfo
func (s emptyStatDir) Mode() os.FileMode  { return 0222 | os.ModeDir }
func (s emptyStatDir) IsDir() bool        { return s.Mode().IsDir() }
func (s emptyStatDir) Name() string       { return string(s) }
func (s emptyStatDir) Sys() interface{}   { return nil }
func (s emptyStatDir) Size() int64        { return 0 }
func (s emptyStatDir) ModTime() time.Time { return time.Time{} }

type emptyFile struct{ emptyStatFile }

var _ styxfile.Interface = emptyFile{}

func (f emptyFile) ReadAt(p []byte, offset int64) (written int, err error) { return 0, io.EOF }
func (f emptyFile) WriteAt(p []byte, offset int64) (int, error)            { return 0, styxfile.ErrNotSupported }
func (f emptyFile) Close() error                                           { return nil }

type emptyDir struct{ emptyStatDir }

var _ styxfile.Directory = emptyDir{}

func (d emptyDir) Readdir(int) ([]os.FileInfo, error) { return nil, nil }

func chanServer(t *testing.T, handler Handler) (in, out chan styxproto.Msg) {
	var ln netutil.PipeListener
	// last for one session
	srv := Server{
		Handler:  handler,
		ErrorLog: testLogger{t},
	}
	go srv.Serve(&ln)
	conn, err := ln.Dial()
	if err != nil {
		panic(err)
	}

	// NOTE(droyo) by buffering the channel we allow the server to take
	// in multiple requests without being blocked on sending their responses.
	// This is a compromise between keeping the messages in order and having
	// an infinite buffer depth (such as with goroutine per channel). Good enough
	// for testing.
	out = make(chan styxproto.Msg, 1000)
	in = make(chan styxproto.Msg)

	go func() {
		for req := range in {
			if _, err := styxproto.Write(conn, req); err != nil {
				t.Error(err)
				break
			}
		}
		conn.Close()
	}()
	go func() {
		d := styxproto.NewDecoder(conn)
		for d.Next() {
			out <- copyMsg(d.Msg())
		}
		close(out)
	}()
	return in, out
}

func copyMsg(msg styxproto.Msg) styxproto.Msg {
	var err error

	rd, wr := io.Pipe()
	d := styxproto.NewDecoder(rd)
	go func() {
		_, err = styxproto.Write(wr, msg)
		wr.CloseWithError(err)
	}()
	for d.Next() {
		return d.Msg()
	}
	panic(fmt.Errorf("failed to copy %T message: %s", msg, d.Err()))
}

func messagesFrom(t *testing.T, r io.Reader) chan styxproto.Msg {
	c := make(chan styxproto.Msg)
	input := styxproto.NewDecoder(r)
	go func() {
		for input.Next() {
			if b, ok := input.Msg().(styxproto.BadMessage); ok {
				t.Logf("skipping bad message: %s", b.Err)
			} else {
				c <- copyMsg(input.Msg())
			}
		}
		if input.Err() != nil {
			t.Error("error reading input: ", input.Err())
		}
		close(c)
	}()
	return c
}

func (s testServer) run(r io.Reader) {
	if s.callback == nil {
		s.callback = func(q, r styxproto.Msg) {}
	}
	pending := make(map[uint16]styxproto.Msg)
	requests, responses := chanServer(s.test, s.handler)

Loop:
	for msg := range messagesFrom(s.test, r) {
		for _, ok := pending[msg.Tag()]; ok; _, ok = pending[msg.Tag()] {
			rsp, ok := <-responses
			if !ok {
				break Loop
			}
			s.test.Logf("\t← %03d %s", rsp.Tag(), rsp)
			if req, ok := pending[rsp.Tag()]; ok {
				s.callback(req, rsp)
				delete(pending, rsp.Tag())
				if flush, ok := req.(styxproto.Tflush); ok {
					if _, ok := rsp.(styxproto.Rflush); ok {
						delete(pending, flush.Oldtag())
					}
				}
			} else {
				s.test.Errorf("Got %T response for unknown tag %d",
					rsp, rsp.Tag())
			}
		}
		s.test.Logf("\t→ %03d %s", msg.Tag(), msg)
		requests <- msg
		pending[msg.Tag()] = msg
	}
Remaining:
	for {
		select {
		case rsp, ok := <-responses:
			if !ok {
				break Remaining
			}
			s.test.Logf("\t← %03d %s", rsp.Tag(), rsp)
			if req, ok := pending[rsp.Tag()]; ok {
				s.callback(req, rsp)
				delete(pending, req.Tag())
			} else {
				s.test.Errorf("got %T response for unused tag %d", rsp, rsp.Tag())
			}
			if len(pending) == 0 {
				break Remaining
			}
		case <-time.After(time.Second * 5):
			s.test.Error("timeout waiting for server response")
			break Remaining
		}
	}
	close(requests)
	if len(pending) > 0 {
		reqs := make([]string, 0, len(pending))
		for _, msg := range pending {
			reqs = append(reqs, fmt.Sprintf("%03d %s", msg.Tag(), msg))
		}
		s.test.Errorf("the following requests were unanswered:\n%s",
			strings.Join(reqs, "\n"))
	}
}

func (s testServer) runMsg(fn func(*styxproto.Encoder)) {
	rd, wr := io.Pipe()
	e := styxproto.NewEncoder(wr)
	go func() {
		e.Tversion(styxproto.DefaultMaxSize, "9P2000")
		e.Tattach(0, 0, styxproto.NoFid, "", "")
		fn(e)
		e.Flush()
		wr.Close()
	}()
	s.run(rd)
}

func (s testServer) runFile(inputfile string) {
	file, cleanup := openfile(inputfile)
	defer cleanup()
	s.run(file)
}

func TestSample(t *testing.T) {
	s := testServer{test: t}
	s.runFile("styxproto/testdata/sample.client.9p")
}

// The only valid response to a Tflush request is an
// Rflush request, regardless of its success.
// Note the tags used in this session; it tests that a server
// does not send a response to a cancelled message.
func TestRflush(t *testing.T) {
	s := testServer{test: t}
	s.callback = func(req, rsp styxproto.Msg) {
		if _, ok := req.(styxproto.Tflush); ok {
			if _, ok := rsp.(styxproto.Rflush); !ok {
				t.Errorf("got %T response to %T", rsp, req)
			}
		}
	}
	s.runMsg(func(enc *styxproto.Encoder) {
		enc.Twalk(1, 0, 1)
		enc.Tflush(2, 1)
		enc.Tflush(3, 2)
		enc.Tflush(2, 1)
		enc.Tflush(2, 2)
		enc.Tflush(1, 300)
		enc.Tclunk(1, 1)
	})
}

func TestCancel(t *testing.T) {
	srv := testServer{test: t}
	const timeout = time.Millisecond * 200
	srv.handler = HandlerFunc(func(s *Session) {
		for s.Next() {
			switch req := s.Request().(type) {
			case Tstat:
				select {
				case <-time.After(timeout):
					t.Errorf("Tstat not cancelled within %s",
						timeout)
				case <-req.Context().Done():
					t.Logf("request cancelled")
					req.Rerror("cancelled")
				}
			}
		}
	})

	srv.runMsg(func(enc *styxproto.Encoder) {
		enc.Twalk(1, 0, 1)
		enc.Tstat(1, 1)
		enc.Tflush(2, 1)
		enc.Tclunk(1, 1)
	})
}

func TestCancelRead(t *testing.T) {
	srv := testServer{test: t}
	const timeout = time.Millisecond * 300
	closeme := make(chan struct{})
	srv.handler = HandlerFunc(func(s *Session) {
		for s.Next() {
			switch req := s.Request().(type) {
			case Twalk:
				req.Rwalk(&slowFile{}, nil)
			case Topen:
				// blockme is nil, will block reads forever
				req.Ropen(&slowFile{
					name:    path.Base(req.Path()),
					closeme: closeme,
				}, nil)
			}
		}
	})

	done := make(chan struct{})
	go func() {
		srv.runMsg(func(enc *styxproto.Encoder) {
			enc.Twalk(1, 0, 1, "somefile")
			enc.Topen(1, 1, styxproto.OREAD)
			enc.Tread(1, 1, 0, 500)
			enc.Tflush(2, 1)
			enc.Tclunk(1, 1)
		})
		close(done)
	}()
	select {
	case <-closeme:
		t.Logf("cancelled read")
		<-done
	case <-time.After(timeout):
		t.Error("cancel read failed")
	}
}

func blankStat(name, uid, gid string) styxproto.Stat {
	buf := make([]byte, styxproto.MaxStatLen)
	stat, _, err := styxproto.NewStat(buf, name, uid, gid, uid)
	if err != nil {
		panic(err)
	}

	stat.SetAtime(maxuint32)
	stat.SetMtime(maxuint32)
	stat.SetDev(maxuint32)
	stat.SetLength(-1)
	stat.SetMode(maxuint32)
	for i := range stat.Qid() {
		stat.Qid()[i] = 0xff
	}
	stat.SetType(maxuint16)
	return stat
}

func TestTwstat(t *testing.T) {
	seen := make(map[string]struct{})
	srv := testServer{test: t}
	srv.handler = HandlerFunc(func(s *Session) {
		for s.Next() {
			switch req := s.Request().(type) {
			case Trename:
				seen["Trename"] = struct{}{}
				req.Rrename(nil)
			case Tchmod:
				seen["Tchmod"] = struct{}{}
				req.Rchmod(nil)
			case Ttruncate:
				req.Rtruncate(nil)
				seen["Ttruncate"] = struct{}{}
			case Tchown:
				req.Rchown(nil)
				seen["Tchown"] = struct{}{}
			case Tutimes:
				req.Rutimes(nil)
				seen["Tutimes"] = struct{}{}
			case Tsync:
				req.Rsync(nil)
				seen["Tsync"] = struct{}{}
			}
		}
	})
	srv.runMsg(func(enc *styxproto.Encoder) {
		var (
			statblank  = blankStat("", "", "")
			statrename = blankStat("newname", "", "")
			statchown  = blankStat("", "newuser", "newgroup")
		)
		enc.Twalk(1, 0, 1)
		{
			// Tutimes
			statblank.SetAtime(uint32(time.Now().Unix()))
			enc.Twstat(1, 1, statblank)
			statblank.SetAtime(maxuint32)
		}
		{
			// Tchmod
			statblank.SetMode(0777)
			enc.Twstat(1, 1, statblank)
			statblank.SetMode(maxuint32)
		}
		{
			// Ttruncate
			statblank.SetLength(100)
			enc.Twstat(1, 1, statblank)
			statblank.SetLength(-1)
		}
		// Tsync
		enc.Twstat(1, 1, statblank)

		enc.Twstat(1, 1, statrename)
		enc.Twstat(1, 1, statchown)
	})
	keys := make([]string, 0, len(seen))
	for k := range seen {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	got := strings.Join(keys, ", ")
	if len(seen) < 6 {
		t.Error("twstat messages did not generate synthentic msg for each field, got", got)
	} else {
		t.Logf("effected %s requests via Twstat messages", got)
	}
}

func TestWalk(t *testing.T) {
	var count int
	srv := testServer{test: t}
	srv.callback = func(req, rsp styxproto.Msg) {
		if _, ok := req.(styxproto.Twalk); ok {
			if _, ok := rsp.(styxproto.Rwalk); !ok {
				t.Errorf("got %T response to %T", rsp, req)
			}
		}
	}
	srv.handler = HandlerFunc(func(s *Session) {
		for s.Next() {
			switch req := s.Request().(type) {
			case Twalk:
				count++
				t.Logf("Twalk %s", req.Path())
				if !strings.HasPrefix(req.Path(), "/") {
					t.Errorf("got non-absolute Twalk path %s", req.Path())
				}
				if strings.Contains(req.Path(), "..") {
					t.Errorf("got non-cleaned Twalk path %s", req.Path())
				}
				req.Rwalk(os.Stat("/"))
			}
		}
	})

	walkPath := "foo/../foo/bar/bar/../../foo"
	elem := strings.Split(walkPath, "/")
	srv.runMsg(func(enc *styxproto.Encoder) {
		enc.Twalk(1, 0, 1, elem...)
	})

	if count != len(elem) {
		t.Errorf("Twalk(%q) generated %d, requests, wanted %d", walkPath, count, len(elem))
	}
}

func TestTcreate(t *testing.T) {
	srv := testServer{test: t}

	type expectedstat struct {
		name string
		mode os.FileMode
	}
	fidnames := map[uint32]expectedstat{
		1: {name: "dir", mode: 0222 | os.ModeDir},
		2: {name: "file", mode: 0222},
	}

	srv.callback = func(req, rsp styxproto.Msg) {
		if _, ok := rsp.(styxproto.Rerror); ok {
			t.Errorf("got %T response to %T", rsp, req)
		}
		if req, ok := req.(styxproto.Tstat); ok {
			if rsp, ok := rsp.(styxproto.Rstat); !ok {
				t.Errorf("got %T response to %T", rsp, req)
			} else {
				expected := fidnames[req.Fid()]
				name := string(rsp.Stat().Name())
				if name != expected.name {
					t.Errorf("expected name to be %s, instead got %s", expected.name, name)
				}
				// FIXME: For directories, the mode does not match
				mode := styxfile.ModeOS(rsp.Stat().Mode())
				if mode != expected.mode {
					t.Errorf("expected mode to be %s, instead got %s", expected.mode, mode)
				}
			}
		}
	}
	srv.handler = HandlerFunc(func(s *Session) {
		for s.Next() {
			switch req := s.Request().(type) {
			case Tcreate:
				t.Logf("Tcreate %s %s", req.Path(), req.NewPath())
				var f any
				if req.Mode.IsDir() {
					f = emptyDir{emptyStatDir(req.Name)}
				} else {
					f = emptyFile{emptyStatFile(req.Name)}
				}
				req.Rcreate(f, nil)
			case Twalk:
				// Empty walks get automatically handled, no need to handle
			case Tstat:
				// Because Rcreate returns an opened file, Tstat is called on styxfile.Interface or styxfile.Directory,
				// so it will use styxfile.Stat to get stat, no need to handle
			}
		}
	})

	srv.runMsg(func(enc *styxproto.Encoder) {
		enc.Twalk(1, 0, 1)
		enc.Tcreate(1, 1, "dir", 0222|styxproto.DMDIR, styxproto.DMREAD)
		enc.Tstat(1, 1)
		enc.Twalk(1, 0, 2)
		enc.Tcreate(1, 2, "file", 0222, styxproto.DMREAD)
		enc.Tstat(1, 2)
	})
}

func TestWalkNonexistent(t *testing.T) {
	srv := testServer{test: t}
	srv.callback = func(req, rsp styxproto.Msg) {
		if _, ok := req.(styxproto.Twalk); ok {
			if _, ok := rsp.(styxproto.Rerror); !ok {
				t.Errorf("expected Rerror response to nonexistent Twalk, instead got: %T", rsp)
			}
		}
	}
	srv.handler = HandlerFunc(func(s *Session) {
		for s.Next() {
			switch req := s.Request().(type) {
			case Twalk:
				t.Logf("Twalk %s", req.Path())
				req.Rwalk(nil, errors.New("not found"))
				// If the walk resulted in an error, then no Qid should have been created for this path
				if _, ok := s.conn.qidpool.Get(req.Path()); ok {
					t.Error("qid was created when it shouldn't have been")
				}
			}
		}
	})

	srv.runMsg(func(enc *styxproto.Encoder) {
		enc.Twalk(1, 0, 1, "nonexistent")
	})
}

func blankQid() styxproto.Qid {
	buf := make([]byte, styxproto.QidLen)
	qid, _, err := styxproto.NewQid(buf, 0, 0, 0)
	if err != nil {
		panic(err)
	}
	return qid
}

// Tests that the Qid of the root folder is the same whether it's
// got via Tattach, Tstat, Topen or Twalk
func TestRootQid(t *testing.T) {
	var numChecks int
	isRootFid := make(map[uint32]bool)
	rootQid := blankQid()

	// T-message that specifies a file ID (FID)
	type fcall interface {
		styxproto.Msg
		Fid() uint32
	}
	// R-message that specifies a file ID (QID)
	type rqid interface {
		styxproto.Msg
		Qid() styxproto.Qid
	}

	srv := testServer{test: t}
	srv.callback = func(req, rsp styxproto.Msg) {
		if m, ok := req.(styxproto.Tattach); ok {
			isRootFid[m.Fid()] = true
		}
		// Track clones of the root fid
		if m, ok := req.(styxproto.Twalk); ok {
			if m.Nwname() == 0 && isRootFid[m.Fid()] {
				isRootFid[m.Newfid()] = true
			}
		}
		if m, ok := req.(styxproto.Tclunk); ok {
			delete(isRootFid, m.Fid())
		}
		if m, ok := rsp.(styxproto.Rattach); ok {
			copy(rootQid, m.Qid())
			t.Logf("root qid detected as %s", rootQid)
		}

		if m, ok := req.(fcall); ok {
			var qid styxproto.Qid
			switch r := rsp.(type) {
			case styxproto.Rstat:
				qid = r.Stat().Qid()
			case rqid:
				qid = r.Qid()
			}
			if qid != nil {
				numChecks++
				if isRootFid[m.Fid()] && !bytes.Equal(qid, rootQid) {
					t.Errorf("root qid is %s, but %T request for %d got response %T %q",
						rootQid, m, m.Fid(), rsp, qid)
				}
			}
		}
	}

	srv.handler = emptyFS(0)

	srv.runMsg(func(enc *styxproto.Encoder) {
		enc.Twalk(1, 0, 1)
		enc.Twalk(1, 1, 2)
		enc.Twalk(1, 1, 3)
		enc.Topen(1, 3, styxproto.OREAD)
		enc.Tclunk(1, 3)
		enc.Tstat(1, 2)
	})
	if numChecks == 0 {
		t.Error("test cases did not fire")
	}
}
