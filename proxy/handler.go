package proxy

import (
	"crypto/tls"
	"encoding/base64"
	"errors"
	"fmt"
	"net"
	"sync"

	"github.com/haxii/fastproxy/bufiopool"
	"github.com/haxii/fastproxy/cert"
	"github.com/haxii/fastproxy/client"
	"github.com/haxii/fastproxy/transport"
	"github.com/haxii/fastproxy/util"
)

//SuperProxy chaining proxy
type SuperProxy struct {
	hostWithPort string
	proxyHeader  string
	secure       bool
}

//NewSuperProxy new a super proxy
func NewSuperProxy(host string, port uint16, ssl bool,
	user string, pass string) (*SuperProxy, error) {
	basicAuth := func(username, password string) string {
		auth := username + ":" + password
		return base64.StdEncoding.EncodeToString([]byte(auth))
	}
	if len(host) == 0 {
		return nil, errors.New("nil host provided")
	}
	if port == 0 {
		return nil, errors.New("nil port provided")
	}
	s := &SuperProxy{secure: ssl}
	s.hostWithPort = fmt.Sprintf("%s:%d", host, port)
	if len(user) > 0 && len(pass) > 0 {
		s.proxyHeader = "Proxy-Authorization: Basic " +
			basicAuth(user, pass) + "\r\n"
	} else {
		s.proxyHeader = ""
	}
	return s, nil
}

//Handler proxy handler
type Handler struct {
	//HTTPSDecryptEnable test if host's https connection should be decrypted
	ShouldDecryptHost func(host string) bool
	//URLProxy url specified proxy
	URLProxy func(uri []byte) *SuperProxy
	//MitmCACert HTTPSDecryptCACert ca.cer used for https decryption
	MitmCACert *tls.Certificate
}

func (h *Handler) handleHTTPConns(c net.Conn, req *Request,
	bufioPool *bufiopool.Pool, sniffer Sniffer, client *client.Client) error {
	//set requests proxy
	req.SetProxy(h.URLProxy(req.reqLine.RawURI()))
	//convert c into a http response
	writer := bufioPool.AcquireWriter(c)
	defer bufioPool.ReleaseWriter(writer)
	defer writer.Flush()
	resp := AcquireResponse()
	defer ReleaseResponse(resp)
	if err := resp.InitWithWriter(writer, sniffer); err != nil {
		return err
	}

	//handle http proxy request
	if err := client.Do(req, resp); err != nil {
		return err
	}
	return nil
}

func (h *Handler) handleHTTPSConns(c net.Conn, hostWithPort string,
	bufioPool *bufiopool.Pool, sniffer Sniffer, client *client.Client) error {
	if h.ShouldDecryptHost(hostWithPort) {
		return h.decryptConnect(c, hostWithPort, bufioPool, sniffer, client)
	}
	return h.tunnelConnect(c, hostWithPort)
}

func (h *Handler) sendHTTPSProxyStatusOK(c net.Conn) (err error) {
	return util.WriteWithValidation(c, []byte("HTTP/1.1 200 OK\r\n\r\n"))
}

func (h *Handler) sendHTTPSProxyStatusBadGateway(c net.Conn) (err error) {
	return util.WriteWithValidation(c, []byte("HTTP/1.1 501 Bad Gateway\r\n\r\n"))
}

//proxy https traffic directly
func (h *Handler) tunnelConnect(conn net.Conn, host string) error {
	errorWrapper := func(msg string, err error) error {
		return fmt.Errorf("%s: %s", msg, err)
	}
	//acquire server conn to target host
	tunnelConn, err := transport.Dial(host)
	if err != nil {
		h.sendHTTPSProxyStatusBadGateway(conn)
		return errorWrapper("error occurred when dialing to host"+host, err)
	}
	defer tunnelConn.Close()

	//handshake with client
	if err := h.sendHTTPSProxyStatusOK(conn); err != nil {
		return errorWrapper("error occurred when handshaking with client", err)
	}
	var wg sync.WaitGroup
	var err1, err2 error
	wg.Add(2)
	go func(e error) {
		err1 = transport.Forward(tunnelConn, conn)
		wg.Done()
	}(err1)
	go func(e error) {
		err2 = transport.Forward(conn, tunnelConn)
		wg.Done()
	}(err2)
	wg.Wait()
	if err1 != nil {
		return errorWrapper("error occurred when tunneling client request to client", err1)
	}
	if err2 != nil {
		return errorWrapper("error occurred when tunneling client response to client", err2)
	}
	return nil
}

//proxy the https connetions by MITM
func (h *Handler) decryptConnect(c net.Conn, hostWithPort string,
	bufioPool *bufiopool.Pool, sniffer Sniffer, client *client.Client) error {
	errorWrapper := func(msg string, err error) error {
		return fmt.Errorf("%s: %s", msg, err)
	}
	//fakeTargetServer means a fake target server for remote client
	//make a connection with client by creating a fake target server
	//
	//make a fake target server's certificate
	fakeTargetServerCert, err := h.signFakeCert(h.MitmCACert, hostWithPort)
	if err != nil {
		h.sendHTTPSProxyStatusBadGateway(c)
		return errorWrapper("error occurred when signing fake certificate for client", err)
	}
	//make the target server's config with this fake certificate
	targetServerName := ""
	fakeTargetServerTLSConfig := &tls.Config{
		Certificates: []tls.Certificate{*fakeTargetServerCert},
		GetCertificate: func(hello *tls.ClientHelloInfo) (*tls.Certificate, error) {
			targetServerName = hello.ServerName
			return cert.GenCert(h.MitmCACert, []string{hello.ServerName})
		},
	}
	//perform the proxy hand shake and fake tls handshake
	handShake := func() (*tls.Conn, error) {
		//make the proxy handshake
		if err := h.sendHTTPSProxyStatusOK(c); err != nil {
			return nil, fmt.Errorf("proxy handshaking error: %s", err)
		}
		//make the tls handshake in https
		conn := tls.Server(c, fakeTargetServerTLSConfig)
		if err := conn.Handshake(); err != nil {
			conn.Close()
			return nil, fmt.Errorf("fake server tls handshaking error: %s", err)
		}
		return conn, nil
	}
	fakeServerConn, err := handShake()
	if len(targetServerName) == 0 {
		err = errors.New("client didn't provide a target server name")
	}
	if err != nil {
		return errorWrapper("error occurred when handshaking with client", err)
	}
	defer fakeServerConn.Close()

	//make a connection with target server by creating a fake remote client
	//
	//convert fakeServerConn into a http request
	reader := bufioPool.AcquireReader(fakeServerConn)
	defer bufioPool.ReleaseReader(reader)
	req := AcquireRequest()
	defer ReleaseRequest(req)
	if err := req.InitWithTLSClientReader(reader,
		sniffer, targetServerName); err != nil {
		return errorWrapper("fail to read MITMed https request header", err)
	}
	//mandatory for tls request cause non hosts provided in request header
	req.SetHostWithPort(hostWithPort)
	//convert fakeServerConn into a http response
	writer := bufioPool.AcquireWriter(fakeServerConn)
	defer bufioPool.ReleaseWriter(writer)
	defer writer.Flush()
	resp := AcquireResponse()
	defer ReleaseResponse(resp)
	if err := resp.InitWithWriter(writer, sniffer); err != nil {
		return errorWrapper("fail to init MITMed https response header", err)
	}
	//handle fake https client request
	if e := client.Do(req, resp); e != nil {
		return errorWrapper("fail to make MITMed https client request ", e)
	}
	return nil
}

func (h *Handler) signFakeCert(mitmCACert *tls.Certificate, host string) (*tls.Certificate, error) {
	domain, _, err := net.SplitHostPort(host)
	if err != nil {
		return nil, fmt.Errorf("get host's %s domain with error %s", host, err)
	}
	cert, err2 := cert.GenCert(mitmCACert, []string{domain})
	if err2 != nil {
		return nil, fmt.Errorf("sign %s fake cert with error %s", domain, err2)
	}
	return cert, nil
}
