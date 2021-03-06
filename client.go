package zrpc

import (
	"bufio"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"github.com/vlzx/zrpc/codec"
	"io"
	"log"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"
)

type Call struct {
	Seq           uint64
	ServiceMethod string
	Args          interface{}
	Reply         interface{}
	Error         error
	Done          chan *Call
}

func (call *Call) done() {
	call.Done <- call
}

type Client struct {
	c        codec.Codec
	opt      *Option
	sending  sync.Mutex // protect sending request
	header   codec.Header
	mux      sync.Mutex // protect Client fields
	seq      uint64
	pending  map[uint64]*Call
	closing  bool
	shutdown bool
}

var _ io.Closer = (*Client)(nil)

var ErrShutdown = errors.New("connection has shut down")

func (client *Client) Close() error {
	client.mux.Lock()
	defer client.mux.Unlock()
	if client.closing {
		return ErrShutdown
	}
	client.closing = true
	return client.c.Close()
}

func (client *Client) IsAvailable() bool {
	client.mux.Lock()
	defer client.mux.Unlock()
	return !client.closing && !client.shutdown
}

func (client *Client) registerCall(call *Call) (uint64, error) {
	client.mux.Lock()
	defer client.mux.Unlock()
	if client.closing || client.shutdown {
		return 0, ErrShutdown
	}
	call.Seq = client.seq
	client.pending[call.Seq] = call
	client.seq++
	return call.Seq, nil
}

func (client *Client) removeCall(seq uint64) *Call {
	client.mux.Lock()
	defer client.mux.Unlock()
	call := client.pending[seq]
	delete(client.pending, seq)
	return call
}

func (client *Client) terminateCalls(err error) {
	client.mux.Lock()
	defer client.mux.Unlock()
	client.sending.Lock()
	defer client.sending.Unlock()
	client.shutdown = true
	for _, call := range client.pending {
		call.Error = err
		call.done()
	}
}

func (client *Client) receive() {
	var err error
	for err == nil {
		var header codec.Header
		err = client.c.ReadHeader(&header)
		if err != nil {
			break
		}
		call := client.removeCall(header.Seq)
		switch {
		case call == nil:
			err = client.c.ReadBody(nil)
		case header.Error != "":
			call.Error = fmt.Errorf(header.Error)
			err = client.c.ReadBody(nil)
			call.done()
		default:
			err = client.c.ReadBody(call.Reply)
			if err != nil {
				call.Error = errors.New("reading body" + err.Error())
			}
			call.done()
		}
	}
	//error occurs, terminate all pending calls
	client.terminateCalls(err)
}

func (client *Client) send(call *Call) {
	client.sending.Lock()
	defer client.sending.Unlock()

	seq, err := client.registerCall(call)
	if err != nil {
		call.Error = err
		call.done()
		return
	}
	client.header.ServiceMethod = call.ServiceMethod
	client.header.Seq = seq
	client.header.Error = ""

	err = client.c.Write(&client.header, call.Args)
	if err != nil {
		call = client.removeCall(seq)
		if call != nil {
			call.Error = err
			call.done()
		}
	}
}

func NewClient(conn net.Conn, opt *Option) (*Client, error) {
	f := codec.NewCodecFuncMap[opt.CodecType]
	if f == nil {
		err := fmt.Errorf("invalid codec type %d", opt.CodecType)
		log.Println("rpc client: codec error:", err)
		return nil, err
	}
	//if err := json.NewEncoder(conn).Encode(opt); err != nil {
	//	log.Println("rpc client: option error:", err)
	//	_ = conn.Close()
	//	return nil, err
	//}
	protocol := []uint64{
		opt.MagicNumber,
		opt.CodecType,
		uint64(opt.ConnectTimeout),
	}
	err := binary.Write(conn, binary.BigEndian, protocol)
	if err != nil {
		log.Println("rpc client: option error:", err)
		_ = conn.Close()
		return nil, err
	}
	return NewClientWithCodec(f(conn), opt), nil
}

func NewClientWithCodec(c codec.Codec, opt *Option) *Client {
	client := &Client{
		c:       c,
		opt:     opt,
		seq:     1,
		pending: make(map[uint64]*Call),
	}
	go client.receive()
	return client
}

func parseOptions(opts ...*Option) (*Option, error) {
	if len(opts) == 0 || opts[0] == nil {
		return DefaultOption, nil
	}
	if len(opts) > 1 {
		return nil, errors.New(fmt.Sprintf("%d options, only 1 is needed", len(opts)))
	}
	opt := opts[0]
	opt.MagicNumber = DefaultOption.MagicNumber
	if opt.CodecType == 0 {
		opt.CodecType = DefaultOption.CodecType
	}
	return opt, nil
}

type clientResult struct {
	client *Client
	err    error
}

type newClientFunc func(conn net.Conn, opt *Option) (*Client, error)

func dialTimeout(f newClientFunc, network string, address string, opts ...*Option) (*Client, error) {
	opt, err := parseOptions(opts...)
	if err != nil {
		return nil, err
	}
	conn, err := net.DialTimeout(network, address, opt.ConnectTimeout)
	if err != nil {
		return nil, err
	}
	defer func() {
		if err != nil {
			_ = conn.Close()
		}
	}()
	ch := make(chan clientResult, 1)
	go func() {
		client, err := f(conn, opt)
		ch <- clientResult{client: client, err: err}
	}()
	if opt.ConnectTimeout == 0 {
		result := <-ch
		return result.client, result.err
	}
	select {
	case <-time.After(opt.ConnectTimeout):
		return nil, fmt.Errorf("rpc client: connect timeout: expect within %s", opt.ConnectTimeout)
	case result := <-ch:
		return result.client, result.err
	}
}

func Dial(network string, address string, opts ...*Option) (*Client, error) {
	return dialTimeout(NewClient, network, address, opts...)
}

func (client *Client) Go(serviceMethod string, args interface{}, reply interface{}, done chan *Call) *Call {
	if done == nil {
		done = make(chan *Call, 10)
	} else if cap(done) == 0 {
		log.Panic("rpc client: done channel is unbuffered")
	}
	call := &Call{
		ServiceMethod: serviceMethod,
		Args:          args,
		Reply:         reply,
		Done:          done,
	}
	client.send(call)
	return call
}

func (client *Client) Call(serviceMethod string, args interface{}, reply interface{}, ctx ...context.Context) error {
	defaultCtx := context.Background()
	if len(ctx) == 1 && ctx[0] != nil {
		defaultCtx = ctx[0]
	}
	call := client.Go(serviceMethod, args, reply, make(chan *Call, 1))
	select {
	case <-defaultCtx.Done():
		client.removeCall(call.Seq)
		return errors.New("rpc client: call timeout: " + defaultCtx.Err().Error())
	case call := <-call.Done:
		return call.Error
	}
}

func NewClientWithHTTP(conn net.Conn, opt *Option) (*Client, error) {
	_, _ = io.WriteString(conn, fmt.Sprintf("CONNECT %s HTTP/1.0\n\n", defaultRPCPath))
	resp, err := http.ReadResponse(bufio.NewReader(conn), &http.Request{Method: "CONNECT"})
	if err == nil {
		if resp.Status == connected {
			return NewClient(conn, opt)
		}
		err = errors.New("unexpected HTTP response: " + resp.Status)
	}
	return nil, err
}

func DialHTTP(network string, address string, opts ...*Option) (*Client, error) {
	return dialTimeout(NewClientWithHTTP, network, address, opts...)
}

func XDial(rpcAddr string, opts ...*Option) (*Client, error) {
	tokens := strings.Split(rpcAddr, "@")
	if len(tokens) != 2 {
		return nil, fmt.Errorf("rpc client: wrong zRPC address format '%s', expect protocol@addr", rpcAddr)
	}
	protocol, addr := tokens[0], tokens[1]
	switch protocol {
	case "http":
		return DialHTTP("tcp", addr, opts...)
	case "tcp":
		return Dial("tcp", addr, opts...)
	default:
		return nil, fmt.Errorf("rpc client: unsupported protocol %s, expect http or tcp", protocol)
	}
}
