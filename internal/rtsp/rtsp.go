package rtsp

import (
	"bufio"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
)

// Method represents an RTSP method.
type Method string

const (
	MethodOptions      Method = "OPTIONS"
	MethodDescribe     Method = "DESCRIBE"
	MethodSetup        Method = "SETUP"
	MethodPlay         Method = "PLAY"
	MethodPause        Method = "PAUSE"
	MethodTeardown     Method = "TEARDOWN"
	MethodAnnounce     Method = "ANNOUNCE"
	MethodRecord       Method = "RECORD"
	MethodGetParameter Method = "GET_PARAMETER"
	MethodSetParameter Method = "SET_PARAMETER"
)

const (
	StatusOK                   = 200
	StatusBadRequest           = 400
	StatusUnauthorized         = 401
	StatusNotFound             = 404
	StatusMethodNotAllowed     = 405
	StatusSessionNotFound      = 454
	StatusUnsupportedTransport = 461
	StatusInternalServerError  = 500
	StatusNotImplemented       = 501
)

// Request represents an RTSP request.
type Request struct {
	Method Method
	URL    *url.URL
	Proto  string // e.g. "RTSP/1.0"
	Header http.Header
	Body   io.ReadCloser
}

// Response represents an RTSP response.
type Response struct {
	Status     string // e.g. "200 OK"
	StatusCode int    // e.g. 200
	Proto      string // e.g. "RTSP/1.0"
	Header     http.Header
	Body       io.ReadCloser
	Request    *Request
}

// InterleavedFrame represents an interleaved RTP/RTCP frame.
type InterleavedFrame struct {
	Channel uint8
	Payload []byte
}

// NewResponse creates a new RTSP response.
func NewResponse(statusCode int, request *Request) *Response {
	return &Response{
		StatusCode: statusCode,
		Status:     fmt.Sprintf("%d %s", statusCode, statusText(statusCode)),
		Proto:      "RTSP/1.0",
		Header:     make(http.Header),
		Request:    request,
	}
}

func statusText(code int) string {
	switch code {
	case 200:
		return "OK"
	case 400:
		return "Bad Request"
	case 401:
		return "Unauthorized"
	case 404:
		return "Not Found"
	case 405:
		return "Method Not Allowed"
	case 454:
		return "Session Not Found"
	case 461:
		return "Unsupported Transport"
	case 500:
		return "Internal Server Error"
	case 501:
		return "Not Implemented"
	default:
		return "Unknown"
	}
}

// ReadRequest reads and parses an RTSP request from b.
func ReadRequest(b *bufio.Reader) (*Request, error) {
	line, err := readLine(b)
	if err != nil {
		return nil, err
	}

	parts := strings.Split(line, " ")
	if len(parts) != 3 {
		return nil, fmt.Errorf("malformed request line: %q", line)
	}

	u, err := url.Parse(parts[1])
	if err != nil {
		return nil, err
	}

	req := &Request{
		Method: Method(parts[0]),
		URL:    u,
		Proto:  parts[2],
		Header: make(http.Header),
	}

	if err := readHeader(b, req.Header); err != nil {
		return nil, err
	}

	if cl := req.Header.Get("Content-Length"); cl != "" {
		n, err := strconv.ParseInt(cl, 10, 64)
		if err != nil {
			return nil, err
		}
		req.Body = io.NopCloser(io.LimitReader(b, n))
	}

	return req, nil
}

// ReadResponse reads and parses an RTSP response from b.
func ReadResponse(b *bufio.Reader, req *Request) (*Response, error) {
	line, err := readLine(b)
	if err != nil {
		return nil, err
	}

	parts := strings.SplitN(line, " ", 3)
	if len(parts) < 2 {
		return nil, fmt.Errorf("malformed response line: %q", line)
	}

	code, err := strconv.Atoi(parts[1])
	if err != nil {
		return nil, err
	}

	resp := &Response{
		Proto:      parts[0],
		StatusCode: code,
		Status:     strings.Join(parts[1:], " "),
		Header:     make(http.Header),
		Request:    req,
	}

	if err := readHeader(b, resp.Header); err != nil {
		return nil, err
	}

	if cl := resp.Header.Get("Content-Length"); cl != "" {
		n, err := strconv.ParseInt(cl, 10, 64)
		if err != nil {
			return nil, err
		}
		resp.Body = io.NopCloser(io.LimitReader(b, n))
	}

	return resp, nil
}

func readLine(b *bufio.Reader) (string, error) {
	line, isPrefix, err := b.ReadLine()
	if err != nil {
		return "", err
	}
	if isPrefix {
		return "", fmt.Errorf("line too long")
	}
	return string(line), nil
}

func readHeader(b *bufio.Reader, h http.Header) error {
	for {
		line, err := readLine(b)
		if err != nil {
			return err
		}
		if line == "" {
			break
		}
		parts := strings.SplitN(line, ":", 2)
		if len(parts) != 2 {
			return fmt.Errorf("malformed header line: %q", line)
		}
		h.Add(strings.TrimSpace(parts[0]), strings.TrimSpace(parts[1]))
	}
	return nil
}

// Write writes the request to w.
func (r *Request) Write(w io.Writer) error {
	if _, err := fmt.Fprintf(w, "%s %s %s\r\n", r.Method, r.URL.String(), r.Proto); err != nil {
		return err
	}
	if err := r.Header.Write(w); err != nil {
		return err
	}
	if _, err := fmt.Fprintf(w, "\r\n"); err != nil {
		return err
	}
	if r.Body != nil {
		_, err := io.Copy(w, r.Body)
		return err
	}
	return nil
}

// Write writes the response to w.
func (r *Response) Write(w io.Writer) error {
	if _, err := fmt.Fprintf(w, "%s %d %s\r\n", r.Proto, r.StatusCode, statusText(r.StatusCode)); err != nil {
		return err
	}
	if err := r.Header.Write(w); err != nil {
		return err
	}
	if _, err := fmt.Fprintf(w, "\r\n"); err != nil {
		return err
	}
	if r.Body != nil {
		_, err := io.Copy(w, r.Body)
		return err
	}
	return nil
}
