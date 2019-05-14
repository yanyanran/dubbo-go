package dubbo

import (
	"github.com/dubbo/go-for-apache-dubbo/common"
	"github.com/dubbo/go-for-apache-dubbo/config"
	"strings"
	"sync"
	"time"
)

import (
	"github.com/AlexStocks/getty"
	"github.com/AlexStocks/goext/sync/atomic"
	log "github.com/AlexStocks/log4go"
	"github.com/dubbogo/hessian2"
	jerrors "github.com/juju/errors"
	"gopkg.in/yaml.v2"
)

import (
	"github.com/dubbo/go-for-apache-dubbo/common/constant"
)

var (
	errInvalidCodecType  = jerrors.New("illegal CodecType")
	errInvalidAddress    = jerrors.New("remote address invalid or empty")
	errSessionNotExist   = jerrors.New("session not exist")
	errClientClosed      = jerrors.New("client closed")
	errClientReadTimeout = jerrors.New("client read timeout")

	clientConf *ClientConfig
)

func init() {

	// load clientconfig from consumer_config
	protocolConf := config.GetConsumerConfig().ProtocolConf
	if protocolConf == nil {
		log.Warn("protocol_conf is nil")
		return
	}
	dubboConf := protocolConf.(map[interface{}]interface{})[DUBBO]
	if protocolConf == nil {
		log.Warn("dubboConf is nil")
		return
	}

	dubboConfByte, err := yaml.Marshal(dubboConf)
	if err != nil {
		panic(err)
	}
	conf := &ClientConfig{}
	err = yaml.Unmarshal(dubboConfByte, conf)
	if err != nil {
		panic(err)
	}

	if err := conf.CheckValidity(); err != nil {
		log.Warn(err)
		return
	}

	clientConf = conf
}

func SetClientConf(c ClientConfig) {
	clientConf = &c
}

func GetClientConf() ClientConfig {
	return *clientConf
}

type CallOptions struct {
	// request timeout
	RequestTimeout time.Duration
	// response timeout
	ResponseTimeout time.Duration
	// serial ID
	SerialID SerialID
	Meta     map[interface{}]interface{}
}

type CallOption func(*CallOptions)

func WithCallRequestTimeout(d time.Duration) CallOption {
	return func(o *CallOptions) {
		o.RequestTimeout = d
	}
}

func WithCallResponseTimeout(d time.Duration) CallOption {
	return func(o *CallOptions) {
		o.ResponseTimeout = d
	}
}

func WithCallSerialID(s SerialID) CallOption {
	return func(o *CallOptions) {
		o.SerialID = s
	}
}

func WithCallMeta_All(callMeta map[interface{}]interface{}) CallOption {
	return func(o *CallOptions) {
		o.Meta = callMeta
	}
}

func WithCallMeta(k, v interface{}) CallOption {
	return func(o *CallOptions) {
		if o.Meta == nil {
			o.Meta = make(map[interface{}]interface{})
		}
		o.Meta[k] = v
	}
}

type CallResponse struct {
	Opts      CallOptions
	Cause     error
	Start     time.Time // invoke(call) start time == write start time
	ReadStart time.Time // read start time, write duration = ReadStart - Start
	Reply     interface{}
}

type AsyncCallback func(response CallResponse)

type Client struct {
	conf     ClientConfig
	pool     *gettyRPCClientPool
	sequence gxatomic.Uint64

	pendingLock      sync.RWMutex
	pendingResponses map[SequenceType]*PendingResponse
}

func NewClient() *Client {

	c := &Client{
		pendingResponses: make(map[SequenceType]*PendingResponse),
		conf:             *clientConf,
	}
	c.pool = newGettyRPCClientConnPool(c, clientConf.PoolSize, time.Duration(int(time.Second)*clientConf.PoolTTL))

	return c
}

// call one way
func (c *Client) CallOneway(addr string, svcUrl common.URL, method string, args interface{}, opts ...CallOption) error {
	var copts CallOptions

	for _, o := range opts {
		o(&copts)
	}

	return jerrors.Trace(c.call(CT_OneWay, addr, svcUrl, method, args, nil, nil, copts))
}

// if @reply is nil, the transport layer will get the response without notify the invoker.
func (c *Client) Call(addr string, svcUrl common.URL, method string, args, reply interface{}, opts ...CallOption) error {
	var copts CallOptions

	for _, o := range opts {
		o(&copts)
	}

	ct := CT_TwoWay
	if reply == nil {
		ct = CT_OneWay
	}

	return jerrors.Trace(c.call(ct, addr, svcUrl, method, args, reply, nil, copts))
}

func (c *Client) AsyncCall(addr string, svcUrl common.URL, method string, args interface{},
	callback AsyncCallback, reply interface{}, opts ...CallOption) error {

	var copts CallOptions
	for _, o := range opts {
		o(&copts)
	}

	return jerrors.Trace(c.call(CT_TwoWay, addr, svcUrl, method, args, reply, callback, copts))
}

func (c *Client) call(ct CallType, addr string, svcUrl common.URL, method string,
	args, reply interface{}, callback AsyncCallback, opts CallOptions) error {

	if opts.RequestTimeout == 0 {
		opts.RequestTimeout = c.conf.GettySessionParam.tcpWriteTimeout
	}
	if opts.ResponseTimeout == 0 {
		opts.ResponseTimeout = c.conf.GettySessionParam.tcpReadTimeout
	}

	p := &DubboPackage{}
	p.Service.Path = strings.TrimPrefix(svcUrl.Path, "/")
	p.Service.Target = svcUrl.GetParam(constant.INTERFACE_KEY, "")
	p.Service.Version = svcUrl.GetParam(constant.VERSION_KEY, constant.DEFAULT_VERSION)
	p.Service.Method = method
	p.Service.Timeout = opts.RequestTimeout
	if opts.SerialID == 0 {
		p.Header.SerialID = byte(S_Dubbo)
	} else {
		p.Header.SerialID = byte(opts.SerialID)
	}
	p.Body = args

	var rsp *PendingResponse
	if ct != CT_OneWay {
		rsp = NewPendingResponse()
		rsp.reply = reply
		rsp.callback = callback
		rsp.opts = opts
	}

	var (
		err     error
		session getty.Session
		conn    *gettyRPCClient
	)
	conn, session, err = c.selectSession(addr)
	if err != nil || session == nil {
		log.Warn(err)
		return errSessionNotExist
	}
	defer c.pool.release(conn, err)

	if err = c.transfer(session, p, rsp, opts); err != nil {
		return jerrors.Trace(err)
	}

	if ct == CT_OneWay || callback != nil {
		return nil
	}

	select {
	case <-getty.GetTimeWheel().After(opts.ResponseTimeout):
		err = errClientReadTimeout
		c.removePendingResponse(SequenceType(rsp.seq))
	case <-rsp.done:
		err = rsp.err
	}

	return jerrors.Trace(err)
}

func (c *Client) Close() {
	if c.pool != nil {
		c.pool.close()
	}
	c.pool = nil
}

func (c *Client) selectSession(addr string) (*gettyRPCClient, getty.Session, error) {
	rpcClient, err := c.pool.getGettyRpcClient(DUBBO, addr)
	if err != nil {
		return nil, nil, jerrors.Trace(err)
	}
	return rpcClient, rpcClient.selectSession(), nil
}

func (c *Client) heartbeat(session getty.Session) error {
	return c.transfer(session, nil, NewPendingResponse(), CallOptions{})
}

func (c *Client) transfer(session getty.Session, pkg *DubboPackage,
	rsp *PendingResponse, opts CallOptions) error {

	var (
		sequence uint64
		err      error
	)

	sequence = c.sequence.Add(1)

	if pkg == nil {
		pkg = &DubboPackage{}
		pkg.Body = []interface{}{}
		pkg.Header.Type = hessian.Heartbeat
		pkg.Header.SerialID = byte(S_Dubbo)
	} else {
		pkg.Header.Type = hessian.Request
	}
	pkg.Header.ID = int64(sequence)

	// cond1
	if rsp != nil {
		rsp.seq = sequence
		c.addPendingResponse(rsp)
	}

	err = session.WritePkg(pkg, opts.RequestTimeout)
	if err != nil {
		c.removePendingResponse(SequenceType(rsp.seq))
	} else if rsp != nil { // cond2
		// cond2 should not merged with cond1. cause the response package may be returned very
		// soon and it will be handled by other goroutine.
		rsp.readStart = time.Now()
	}

	return jerrors.Trace(err)
}

func (c *Client) addPendingResponse(pr *PendingResponse) {
	c.pendingLock.Lock()
	defer c.pendingLock.Unlock()
	c.pendingResponses[SequenceType(pr.seq)] = pr
}

func (c *Client) removePendingResponse(seq SequenceType) *PendingResponse {
	c.pendingLock.Lock()
	defer c.pendingLock.Unlock()
	if c.pendingResponses == nil {
		return nil
	}
	if presp, ok := c.pendingResponses[seq]; ok {
		delete(c.pendingResponses, seq)
		return presp
	}
	return nil
}
