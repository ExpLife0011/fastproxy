package proxy

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"math"
	"sync"

	"github.com/haxii/fastproxy/bytebufferpool"
	"github.com/haxii/fastproxy/header"
)

/*
 * implements basic http request & response based on client
 */

//Request http request implementation of http client
type Request struct {
	//reader stores the original raw data of request
	reader *bufio.Reader

	//start line of http request, i.e. request line
	//build from reader
	reqLine header.RequestLine

	//headers info, includes conn close and content length
	header header.Header

	//sniffer, used for recording the http traffic
	sniffer Sniffer

	//TLS request settings
	isTLS         bool
	tlsServerName string
}

// InitWithProxyReader init request with reader
// then parse the start line of the http request
func (r *Request) InitWithProxyReader(reader *bufio.Reader, sniffer Sniffer) error {
	return r.initWithReader(reader, sniffer, false, "", "")
}

// InitWithTLSClientReader init request with reader supports TLS connections
func (r *Request) InitWithTLSClientReader(reader *bufio.Reader,
	sniffer Sniffer, hostWithPort, tlsServerName string) error {
	return r.initWithReader(reader, sniffer, true, hostWithPort, tlsServerName)
}

func (r *Request) initWithReader(reader *bufio.Reader,
	sniffer Sniffer, isTLS bool, hostWithPort, tlsServerName string) error {
	if r.reader != nil {
		return errors.New("request already initialized")
	}

	if reader == nil {
		return errors.New("nil reader provided")
	}

	if isTLS && len(tlsServerName) == 0 {
		return errors.New("empty tls server name provided")
	}

	if err := r.reqLine.Parse(reader, hostWithPort); err != nil {
		if err == header.ErrNoHostProvided {
			return err
		}
		return fmt.Errorf("fail to read start line of request with error %s", err)
	}
	r.reader = reader
	r.sniffer = sniffer
	r.isTLS = isTLS
	r.tlsServerName = tlsServerName
	return nil
}

//GetStartLine return the start line of request
func (r *Request) GetStartLine() header.RequestLine {
	return r.reqLine
}

//WriteTo write raw http request body to http client
//implemented client's request interface
func (r *Request) WriteTo(writer *bufio.Writer) error {
	if r.reader == nil {
		return errors.New("Empty request, nothing to write")
	}

	buffer := bytebufferpool.Get()
	defer bytebufferpool.Put(buffer)

	//rebuild & write the start line
	r.reqLine.RebuildRequestLine(buffer)
	if _, err := writer.Write(buffer.B); err != nil {
		return fmt.Errorf("fail to write start line : %s", err)
	}
	r.sniffer.ReqLine(buffer.B)

	//read & write the headers
	if err := r.header.ParseHeaderFields(r.reader, buffer); err != nil {
		return fmt.Errorf("fail to parse http headers : %s", err)
	}
	if _, err := writer.Write(buffer.B); err != nil {
		return fmt.Errorf("fail to write headers : %s", err)
	}
	r.sniffer.Header(buffer.B)

	//write the request body (if any)
	return copyBody(r.reader, writer, r.header, r.sniffer)
}

// ConnectionClose if the request's "Connection" header value is set as "Close".
// this determines how the client reusing the connetions.
// this func. result is only valid after `WriteTo` method is called
func (r *Request) ConnectionClose() bool {
	return r.header.IsConnectionClose()
}

//Reset reset request
func (r *Request) Reset() {
	r.reader = nil
	r.reqLine.Reset()
	r.header.Reset()
}

//IsIdempotent specified in request's start line usually
func (r *Request) IsIdempotent() bool {
	return r.reqLine.IsIdempotent()
}

//IsTLS is tls requests
func (r *Request) IsTLS() bool {
	return r.isTLS
}

//HostWithPort host/addr target
func (r *Request) HostWithPort() string {
	return r.reqLine.HostWithPort()
}

//TLSServerName server name for handshaking
func (r *Request) TLSServerName() string {
	return r.tlsServerName
}

//Response http response implementation of http client
type Response struct {
	writer  *bufio.Writer
	sniffer Sniffer

	//start line of http response, i.e. request line
	//build from reader
	respLine header.ResponseLine

	//headers info, includes conn close and content length
	header header.Header
}

// InitWithWriter init response with writer
func (r *Response) InitWithWriter(writer *bufio.Writer, sniffer Sniffer) error {
	if r.writer != nil {
		return errors.New("response already initialized")
	}

	if writer == nil {
		return errors.New("nil writer provided")
	}

	r.writer = writer
	r.sniffer = sniffer
	return nil
}

//ReadFrom read data from http response got
func (r *Response) ReadFrom(reader *bufio.Reader) error {
	//write back the start line to writer(i.e. net/connection)
	if err := r.respLine.Parse(reader); err != nil {
		return fmt.Errorf("fail to read start line of response with error %s", err)
	}
	respLineBytes := r.respLine.GetResponseLine()
	if _, err := r.writer.Write(respLineBytes); err != nil {
		return fmt.Errorf("fail to write start line : %s", err)
	}
	r.sniffer.RespLine(respLineBytes)

	buffer := bytebufferpool.Get()
	defer bytebufferpool.Put(buffer)

	//read & write the headers
	if err := r.header.ParseHeaderFields(reader, buffer); err != nil {
		return fmt.Errorf("fail to parse http headers : %s", err)
	}
	if _, err := r.writer.Write(buffer.B); err != nil {
		return fmt.Errorf("fail to write headers : %s", err)
	}
	r.sniffer.Header(buffer.B)

	//write the request body (if any)
	return copyBody(reader, r.writer, r.header, r.sniffer)
}

//Reset reset response
func (r *Response) Reset() {
	r.writer = nil
	r.respLine.Reset()
	r.header.Reset()
}

//ConnectionClose if the request's "Connection" header value is set as "Close"
//this determines how the client reusing the connetions
func (r *Response) ConnectionClose() bool {
	return false
}

var (
	//pool for requests and responses
	requestPool  sync.Pool
	responsePool sync.Pool
)

// AcquireRequest returns an empty Request instance from request pool.
//
// The returned Request instance may be passed to ReleaseRequest when it is
// no longer needed. This allows Request recycling, reduces GC pressure
// and usually improves performance.
func AcquireRequest() *Request {
	v := requestPool.Get()
	if v == nil {
		return &Request{}
	}
	return v.(*Request)
}

// ReleaseRequest returns req acquired via AcquireRequest to request pool.
//
// It is forbidden accessing req and/or its' members after returning
// it to request pool.
func ReleaseRequest(req *Request) {
	req.Reset()
	requestPool.Put(req)
}

// AcquireResponse returns an empty Response instance from response pool.
//
// The returned Response instance may be passed to ReleaseResponse when it is
// no longer needed. This allows Response recycling, reduces GC pressure
// and usually improves performance.
func AcquireResponse() *Response {
	v := responsePool.Get()
	if v == nil {
		return &Response{}
	}
	return v.(*Response)
}

// ReleaseResponse return resp acquired via AcquireResponse to response pool.
//
// It is forbidden accessing resp and/or its' members after returning
// it to response pool.
func ReleaseResponse(resp *Response) {
	resp.Reset()
	responsePool.Put(resp)
}

func copyBody(src *bufio.Reader, dst *bufio.Writer, header header.Header, sniffer Sniffer) error {
	if header.ContentLength() > 0 {
		//read contentLength data more from reader
		return copyBodyFixedSize(src, dst, header.ContentLength(), sniffer)
	} else if header.IsBodyChunked() {
		//read data chunked
		buffer := bytebufferpool.Get()
		defer bytebufferpool.Put(buffer)
		return copyBodyChunked(src, dst, buffer, sniffer)
	} else if header.IsBodyIdentity() {
		//read till eof
		return copyBodyIdentity(src, dst, sniffer)
	}
	return nil
}

func copyBodyFixedSize(src *bufio.Reader, dst *bufio.Writer,
	contentLength int64, sniffer Sniffer) error {
	byteStillNeeded := contentLength
	for {
		//read one more bytes
		if b, _ := src.Peek(1); len(b) == 0 {
			return io.EOF
		}

		//must read buffed bytes
		b, err := src.Peek(src.Buffered())
		if len(b) == 0 || err != nil {
			panic(fmt.Sprintf("bufio.Reader.Peek() returned unexpected data (%q, %v)", b, err))
		}

		//write read bytes into dst
		_bytesShouldRead := int64(len(b))
		if byteStillNeeded <= _bytesShouldRead {
			_bytesShouldRead = byteStillNeeded
		}
		byteStillNeeded -= _bytesShouldRead
		bytesShouldRead := int(_bytesShouldRead)

		bytesShouldWrite, err := dst.Write(b[:bytesShouldRead])
		if err != nil {
			return fmt.Errorf("fail to write request body : %s", err)
		}
		if bytesShouldWrite != bytesShouldRead {
			return io.ErrShortWrite
		}
		sniffer.Body(b[:bytesShouldRead])

		//must discard wrote bytes
		if _, err := src.Discard(bytesShouldWrite); err != nil {
			panic(fmt.Sprintf("bufio.Reader.Discard(%d) failed: %s", bytesShouldWrite, err))
		}

		//test if still read more bytes
		if byteStillNeeded == 0 {
			return nil
		}
	}
}

var strCRLF = []byte("\r\n")

func copyBodyChunked(src *bufio.Reader, dst *bufio.Writer,
	buffer *bytebufferpool.ByteBuffer, sniffer Sniffer) error {
	strCRLFLen := len(strCRLF)

	for {
		//read and calculate chunk size
		buffer.Reset()
		chunkSize, err := parseChunkSize(src, buffer)
		if err != nil {
			return err
		}
		if _, err := dst.Write(buffer.B); err != nil {
			return err
		}
		sniffer.Body(buffer.B)

		//copy the chunk
		if err := copyBodyFixedSize(src, dst,
			int64(chunkSize+strCRLFLen), sniffer); err != nil {
			return err
		}
		if chunkSize == 0 {
			return nil
		}
	}
}

func parseChunkSize(r *bufio.Reader, buffer *bytebufferpool.ByteBuffer) (int, error) {
	n, err := readHexInt(r, buffer)
	if err != nil {
		return -1, err
	}
	c, err := r.ReadByte()
	if err != nil {
		return -1, fmt.Errorf("cannot read '\r' char at the end of chunk size: %s", err)
	}
	if c != '\r' {
		return -1, fmt.Errorf("unexpected char %q at the end of chunk size. Expected %q", c, '\r')
	}
	c, err = r.ReadByte()
	if err != nil {
		return -1, fmt.Errorf("cannot read '\n' char at the end of chunk size: %s", err)
	}
	if c != '\n' {
		return -1, fmt.Errorf("unexpected char %q at the end of chunk size. Expected %q", c, '\n')
	}
	if _, e := buffer.Write([]byte("\r\n")); e != nil {
		return -1, e
	}
	return n, nil
}
func copyBodyIdentity(src *bufio.Reader, dst *bufio.Writer, sniffer Sniffer) error {
	if err := copyBodyFixedSize(src, dst, math.MaxInt64, sniffer); err != nil {
		if err == io.EOF {
			return nil
		}
		return err
	}
	return nil
}
