package client

import (
	"bufio"
	"fmt"
	"io"
	"log"
	"net"
	nethttp "net/http"
	"strings"
	"testing"

	"github.com/haxii/fastproxy/bufiopool"
	"github.com/haxii/fastproxy/bytebufferpool"
	"github.com/haxii/fastproxy/http"
	proxyhttp "github.com/haxii/fastproxy/proxy/http"
	"github.com/haxii/fastproxy/superproxy"
)

func TestClientDo(t *testing.T) {
	go func() {
		nethttp.HandleFunc("/", func(w nethttp.ResponseWriter, r *nethttp.Request) {
			fmt.Fprintf(w, "Hello world,tteterete%s!\r\n", r.URL.Path[1:])
		})
		log.Fatal(nethttp.ListenAndServe(":10000", nil))
	}()
	testClientDo(t, nil)
	superProxy, _ := superproxy.NewSuperProxy("127.0.0.1", 5080, superproxy.ProxyTypeHTTP, "", "", true)
	testClientDo(t, superProxy)
}

func testClientDo(t *testing.T, superProxy *superproxy.SuperProxy) {
	var err error
	bPool := bufiopool.New(bufiopool.MinReadBufferSize, bufiopool.MinWriteBufferSize)
	c := &Client{
		BufioPool: bPool,
	}
	s := "GET / HTTP/1.1\r\n" +
		"Host: localhost:10000\r\n" +
		"\r\n"
	req := &proxyhttp.Request{}
	if superProxy != nil {
		req.SetProxy(superProxy)
	}
	sHijacker := &hijacker{}
	addr := testAddr{netWork: "tcp", clientAddr: "127.0.0.1:10000"}
	var clientAddr net.Addr = &addr
	sHijacker.Set(clientAddr, "localhost", []byte("GET"), []byte("/"))
	req.SetHijacker(sHijacker)
	br := bufio.NewReader(strings.NewReader(s))
	err = req.ReadFrom(br)
	if err != nil {
		t.Fatalf("unexpected error: %s", err.Error())
	}
	req.SetHostWithPort("localhost:10000")
	resp := &proxyhttp.Response{}
	byteBuffer := bytebufferpool.MakeFixedSizeByteBuffer(100)
	bw := bufio.NewWriter(byteBuffer)
	err = resp.WriteTo(bw)
	if err != nil {
		t.Fatalf("unexpected error: %s", err.Error())
	}
	resp.SetHijacker(sHijacker)
	err = c.Do(req, resp)
	if err != nil {
		t.Fatalf("unexpected error : %s", err.Error())
	}
	if resp.GetSize() == 0 {
		t.Fatal("Response can't be empty")
	}
	if bw.Buffered() == 0 {
		t.Fatal("Response don't write to bufio writer")
	}
	defer bw.Flush()
}

type testAddr struct {
	clientAddr string
	netWork    string
}

func (a *testAddr) String() string {
	return a.clientAddr
}

func (a *testAddr) Network() string {
	return a.netWork
}

type hijacker struct {
	clientAddr, targetHost string
	method, path           []byte
}

func (s *hijacker) Set(clientAddr net.Addr,
	host string, method, path []byte) {
	s.clientAddr = clientAddr.String()
	s.targetHost = host
	s.method = method
	s.path = path
}

func (s *hijacker) OnRequest(header http.Header, rawHeader []byte) io.Writer {
	return nil
}

func (s *hijacker) HijackResponse() io.Reader {
	return nil
}

func (s *hijacker) OnResponse(respLine http.ResponseLine,
	header http.Header, rawHeader []byte) io.Writer {
	return nil
}
