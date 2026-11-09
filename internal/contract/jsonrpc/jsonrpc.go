// Package jsonrpc is the JSON-RPC 2.0 framing shared by plugins (D3, I42) and the
// AI worker (I6): one JSON object per line over any byte stream. Having a single
// codec means both speak exactly the same wire format.
package jsonrpc

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strconv"
	"sync"
)

// Version is the protocol version string.
const Version = "2.0"

// Standard error codes, plus the implementation range used by Vidyom.
const (
	CodeParseError     = -32700
	CodeInvalidRequest = -32600
	CodeMethodNotFound = -32601
	CodeInvalidParams  = -32602
	CodeInternalError  = -32603
	CodeRequestCancel  = -32800
	// CodeUnavailable means a required engine or model is not usable.
	CodeUnavailable = -32001
	// CodeUnauthorized means initialize was required or its token was wrong.
	CodeUnauthorized = -32002
)

// MaxMessageBytes bounds a single line. Embeddings of a long recording fit.
const MaxMessageBytes = 64 << 20

// Message is the union of request, response and notification.
type Message struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Method  string          `json:"method,omitempty"`
	Params  json.RawMessage `json:"params,omitempty"`
	Result  json.RawMessage `json:"result,omitempty"`
	Error   *Error          `json:"error,omitempty"`
}

// Error is a JSON-RPC error object.
type Error struct {
	Code    int             `json:"code"`
	Message string          `json:"message"`
	Data    json.RawMessage `json:"data,omitempty"`
}

func (e *Error) Error() string { return fmt.Sprintf("jsonrpc error %d: %s", e.Code, e.Message) }

// Handler answers incoming requests and notifications on a Conn. Return a *Error
// to control the code. A nil Handler refuses requests.
type Handler func(ctx context.Context, method string, params json.RawMessage) (any, error)

// Conn is one bidirectional connection. It is safe for concurrent Calls.
type Conn struct {
	r       *bufio.Reader
	w       io.Writer
	closer  io.Closer
	handler Handler

	writeMu sync.Mutex
	mu      sync.Mutex
	nextID  int64
	pending map[string]chan Message
	closed  chan struct{}
	err     error
}

// NewConn wraps a stream. Call Run to start reading. closer may be nil.
func NewConn(r io.Reader, w io.Writer, closer io.Closer, handler Handler) *Conn {
	return &Conn{
		r:       bufio.NewReaderSize(r, 64<<10),
		w:       w,
		closer:  closer,
		handler: handler,
		pending: map[string]chan Message{},
		closed:  make(chan struct{}),
	}
}

// Run reads messages until the stream ends or ctx ends, and returns why. Handlers
// run in their own goroutines, so a long method never blocks progress
// notifications or cancellation.
func (c *Conn) Run(ctx context.Context) error {
	stop := context.AfterFunc(ctx, func() { c.shutdown(ctx.Err()) })
	defer stop()
	for {
		line, err := readLine(c.r)
		if err != nil {
			c.shutdown(err)
			return c.closeErr()
		}
		if len(line) == 0 {
			continue
		}
		var m Message
		if err := json.Unmarshal(line, &m); err != nil {
			_ = c.send(Message{JSONRPC: Version, ID: json.RawMessage("null"), Error: &Error{Code: CodeParseError, Message: "invalid JSON"}})
			continue
		}
		switch {
		case m.Method != "":
			go c.dispatch(ctx, m)
		case m.ID != nil:
			c.deliver(m)
		}
	}
}

func readLine(r *bufio.Reader) ([]byte, error) {
	var out []byte
	for {
		chunk, err := r.ReadSlice('\n')
		out = append(out, chunk...)
		if len(out) > MaxMessageBytes {
			return nil, errors.New("jsonrpc: message exceeds the size limit")
		}
		if err == nil {
			return out[:len(out)-1], nil
		}
		if !errors.Is(err, bufio.ErrBufferFull) {
			return nil, err
		}
	}
}

func (c *Conn) dispatch(ctx context.Context, m Message) {
	var result any
	var err error
	if c.handler == nil {
		err = &Error{Code: CodeMethodNotFound, Message: "method not found"}
	} else {
		result, err = c.handler(ctx, m.Method, m.Params)
	}
	if m.ID == nil {
		return // notification: no reply, errors are the handler's to log
	}
	reply := Message{JSONRPC: Version, ID: m.ID}
	if err != nil {
		var rpcErr *Error
		if errors.As(err, &rpcErr) {
			reply.Error = rpcErr
		} else {
			reply.Error = &Error{Code: CodeInternalError, Message: err.Error()}
		}
	} else {
		data, marshalErr := json.Marshal(result)
		if marshalErr != nil {
			reply.Error = &Error{Code: CodeInternalError, Message: "cannot encode result"}
		} else {
			reply.Result = data
		}
	}
	_ = c.send(reply)
}

func (c *Conn) deliver(m Message) {
	c.mu.Lock()
	ch, ok := c.pending[string(m.ID)]
	delete(c.pending, string(m.ID))
	c.mu.Unlock()
	if ok {
		ch <- m
	}
}

func (c *Conn) send(m Message) error {
	data, err := json.Marshal(m)
	if err != nil {
		return err
	}
	c.writeMu.Lock()
	defer c.writeMu.Unlock()
	_, err = c.w.Write(append(data, '\n'))
	return err
}

// Call sends a request and waits for its response, decoding into result (which
// may be nil). When ctx ends first it sends $/cancelRequest and returns ctx.Err().
func (c *Conn) Call(ctx context.Context, method string, params, result any) error {
	raw, err := json.Marshal(params)
	if err != nil {
		return fmt.Errorf("encode params: %w", err)
	}
	c.mu.Lock()
	c.nextID++
	idStr := strconv.FormatInt(c.nextID, 10)
	ch := make(chan Message, 1)
	c.pending[idStr] = ch
	c.mu.Unlock()

	if err := c.send(Message{JSONRPC: Version, ID: json.RawMessage(idStr), Method: method, Params: raw}); err != nil {
		c.forget(idStr)
		return err
	}
	select {
	case m := <-ch:
		if m.Error != nil {
			return m.Error
		}
		if result == nil || len(m.Result) == 0 {
			return nil
		}
		return json.Unmarshal(m.Result, result)
	case <-ctx.Done():
		c.forget(idStr)
		_ = c.Notify(context.Background(), MethodCancelRequest, CancelParams{ID: idStr})
		return ctx.Err()
	case <-c.closed:
		c.forget(idStr)
		return c.closeErr()
	}
}

func (c *Conn) forget(idStr string) {
	c.mu.Lock()
	delete(c.pending, idStr)
	c.mu.Unlock()
}

// Notify sends a notification.
func (c *Conn) Notify(_ context.Context, method string, params any) error {
	raw, err := json.Marshal(params)
	if err != nil {
		return err
	}
	return c.send(Message{JSONRPC: Version, Method: method, Params: raw})
}

// Close ends the connection.
func (c *Conn) Close() error {
	c.shutdown(errors.New("jsonrpc: connection closed"))
	return nil
}

func (c *Conn) shutdown(err error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	select {
	case <-c.closed:
		return
	default:
	}
	c.err = err
	close(c.closed)
	if c.closer != nil {
		_ = c.closer.Close()
	}
}

func (c *Conn) closeErr() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.err
}

// Shared notification methods, as in LSP.
const (
	MethodCancelRequest = "$/cancelRequest"
	MethodProgress      = "$/progress"
)

// CancelParams names the request to cancel.
type CancelParams struct {
	ID string `json:"id"`
}

// ProgressParams reports progress of a request.
type ProgressParams struct {
	Token   string  `json:"token"`
	Percent float64 `json:"percent"`
	Message string  `json:"message,omitempty"`
}
