package redislistener

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"sync"
	"time"

	"github.com/go4org/go4/syncutil"
	"github.com/rs/xid"

	"github.com/golang/protobuf/proto"
	"github.com/hashicorp/go-multierror"
	"github.com/mediocregopher/radix/v3"
)

type RedisOpt struct {
	Network  string
	Address  string
	PoolSize int
}

func NewDialer(ctx context.Context, addr string, opt RedisOpt) (*Dialer, error) {
	pubPool, subConn, err := dialConn(opt)
	if err != nil {
		return nil, err
	}

	ctx, cancel := context.WithCancel(ctx)
	pubSub, err := newPubSub(ctx, pubSubAddr(addr), pubPool, subConn)
	if err != nil {
		return nil, err
	}

	return &Dialer{
		id:          xid.New().String(),
		dialChannel: addr,
		pubSub:      pubSub,
		ctx:         ctx,
		cancel:      cancel,
	}, nil
}

type Dialer struct {
	id          string
	dialChannel string
	pubSub      *pubSub

	ctx    context.Context
	cancel context.CancelFunc

	closeOnce syncutil.Once
}

func (d *Dialer) close() error {
	return d.pubSub.Close()
}

func (d *Dialer) Close() error {
	d.cancel()
	return d.closeOnce.Do(d.close)
}

func (d *Dialer) isClosed() bool {
	select {
	case <-d.ctx.Done():
		return true
	default:
		return false
	}
}

func (d *Dialer) Dial() (net.Conn, error) {
	return d.DialContext(d.ctx)
}

func (d *Dialer) DialContext(ctx context.Context) (net.Conn, error) {
	if d.isClosed() {
		return nil, fmt.Errorf("dialer is closed")
	}

	id := xid.New().String()

	msgCh := make(chan *Message)
	if !d.pubSub.Subscribe(id, msgCh) {
		return nil, fmt.Errorf("error subscribing to channel %s in listener dial", id)
	}
	defer d.pubSub.Unsubscribe(id, msgCh)

	m := &Message{
		Kind:     Message_CLIENT_JOIN,
		Channel:  d.dialChannel,
		SenderId: d.id,
		ReplyId:  id,
		Body:     []byte(id),
	}
	if err := rpushMsg(d.pubSub.pubPool, d.dialChannel, m); err != nil {
		return nil, err
	}

	select {
	case msg := <-msgCh:
		return newConn(ctx, id, id, string(msg.Body), d.pubSub), nil
	case <-time.After(10 * time.Second):
		return nil, fmt.Errorf("timeout dialing %s", d.dialChannel)
	}
}

func NewListener(ctx context.Context, addr string, opt RedisOpt) (*Listener, error) {
	pubConn, subConn, err := dialConn(opt)
	if err != nil {
		return nil, err
	}

	ctx, cancel := context.WithCancel(ctx)
	pubSub, err := newPubSub(ctx, pubSubAddr(addr), pubConn, subConn)
	if err != nil {
		return nil, err
	}

	ln := &Listener{
		id:            xid.New().String(),
		listenChannel: addr,
		pubSub:        pubSub,
		acceptCh:      make(chan *Message),
		ctx:           ctx,
		cancel:        cancel,
	}

	go func() {
		_ = ln.controlLoop()
	}()

	return ln, nil
}

type Listener struct {
	id string

	listenChannel string
	pubSub        *pubSub

	acceptCh chan *Message

	ctx    context.Context
	cancel context.CancelFunc

	closeOnce syncutil.Once
}

func (l *Listener) controlLoop() error {
	for {
		if l.isClosed() {
			return l.ctx.Err()
		}

		var val [][]byte

		// slightly less than connection timeout
		if err := l.pubSub.pubPool.Do(radix.Cmd(&val, "BLPOP", l.listenChannel, "4")); err != nil {
			continue
		}

		if len(val) != 2 || len(val[1]) == 0 {
			continue
		}

		m := &Message{}
		if err := proto.Unmarshal(val[1], m); err != nil {
			continue
		}

		if l.isClosed() {
			return l.ctx.Err()
		}

		l.acceptCh <- m
	}
}

func (l *Listener) Accept() (net.Conn, error) {
	if l.isClosed() {
		return nil, errors.New("listener is closed")
	}

	for m := range l.acceptCh {
		id := xid.New().String()
		c := newConn(l.ctx, id, id, string(m.Body), l.pubSub)

		mm := &Message{
			Kind:     Message_SERVER_ACCEPT,
			Channel:  m.ReplyId,
			SenderId: l.id,
			Body:     []byte(id),
		}
		if err := l.pubSub.Publish(mm); err != nil {
			return nil, err
		}

		return c, nil
	}

	return nil, errors.New("listener is closed")
}

func (l *Listener) isClosed() bool {
	select {
	case <-l.ctx.Done():
		return true
	default:
		return false
	}
}

func (l *Listener) close() error {
	if err := l.pubSub.Close(); err != nil {
		return err
	}

	close(l.acceptCh)

	return nil
}

func (l *Listener) Close() error {
	l.cancel()
	return l.closeOnce.Do(l.close)
}

func (l *Listener) Addr() net.Addr { return addr{} }

func newConn(
	ctx context.Context,
	id, rch, wch string,
	pubSub *pubSub,
) *conn {
	ctx, cancel := context.WithCancel(ctx)
	rr, rw := io.Pipe()

	msgCh := make(chan *Message)
	c := &conn{
		id:     id,
		rch:    rch,
		wch:    wch,
		rr:     rr,
		rw:     rw,
		pubSub: pubSub,
		msgCh:  msgCh,
		ctx:    ctx,
		cancel: cancel,
	}
	pubSub.Subscribe(c.rch, c.msgCh)

	go func() {
		_ = c.readLoop()
	}()

	return c
}

type conn struct {
	id string

	rch string
	wch string

	rr *io.PipeReader
	rw *io.PipeWriter

	pubSub *pubSub
	msgCh  chan *Message

	ctx    context.Context
	cancel context.CancelFunc

	closeOnce syncutil.Once
}

func (p *conn) Read(b []byte) (n int, err error) {
	if p.isClosed() {
		return 0, p.ctx.Err()
	}

	return p.rr.Read(b)
}

func (p *conn) Write(b []byte) (n int, err error) {
	if p.isClosed() {
		return 0, p.ctx.Err()
	}

	m := &Message{
		Kind:     Message_DATA,
		Channel:  p.wch,
		SenderId: p.id,
		Body:     b,
	}
	if err := p.pubSub.Publish(m); err != nil {
		return 0, err
	}

	return len(b), nil
}

func (p *conn) isClosed() bool {
	select {
	case <-p.ctx.Done():
		return true
	default:
		return false
	}
}

func (p *conn) close() error {
	p.pubSub.Unsubscribe(p.rch, p.msgCh)
	close(p.msgCh)

	return nil
}

func (p *conn) Close() error {
	p.cancel()

	var result error

	if err := p.closeOnce.Do(p.close); err != nil {
		result = multierror.Append(result, err)
	}

	if err := p.rw.Close(); err != nil {
		result = multierror.Append(result, err)
	}

	if err := p.rr.Close(); err != nil {
		result = multierror.Append(result, err)
	}

	return result
}

func (c *conn) SetDeadline(t time.Time) error {
	return nil
}

func (c *conn) SetReadDeadline(t time.Time) error {
	return nil
}

func (c *conn) SetWriteDeadline(t time.Time) error {
	return nil
}

func (*conn) LocalAddr() net.Addr  { return addr{} }
func (*conn) RemoteAddr() net.Addr { return addr{} }

func (p *conn) readLoop() error {
	for {
		select {
		case m := <-p.msgCh:
			if m == nil {
				continue
			}

			if m.Kind != Message_DATA {
				continue
			}

			if _, err := io.Copy(p.rw, bytes.NewBuffer(m.Body)); err != nil {
				return err
			}
		case <-p.ctx.Done():
			return p.ctx.Err()
		}
	}
}

func newPubSub(
	ctx context.Context,
	channel string,
	pubPool *radix.Pool,
	subConn radix.PubSubConn,
) (*pubSub, error) {
	ctx, cancel := context.WithCancel(ctx)

	r := &pubSub{
		channel:     channel,
		subscribers: make(map[string]map[chan *Message]struct{}),
		pubPool:     pubPool,
		subConn:     subConn,
		subCh:       make(chan radix.PubSubMessage),
		ctx:         ctx,
		cancel:      cancel,
	}

	if err := r.subConn.Subscribe(r.subCh, r.channel); err != nil {
		return nil, err
	}
	go func() {
		_ = r.subLoop()
	}()

	return r, nil
}

type pubSub struct {
	channel string

	subscribers map[string]map[chan *Message]struct{} // channel_name: subscribed chs
	mux         sync.Mutex
	isClose     bool

	pubPool   *radix.Pool
	subConn   radix.PubSubConn
	subCh     chan radix.PubSubMessage
	closeOnce syncutil.Once

	ctx    context.Context
	cancel context.CancelFunc
}

func (b *pubSub) Publish(m *Message) error {
	return pubMsg(b.pubPool, b.channel, m)
}

func (b *pubSub) close() error {
	var result error

	if err := b.pubPool.Close(); err != nil {
		result = multierror.Append(result, err)
	}

	if err := b.subConn.Unsubscribe(b.subCh, b.channel); err != nil {
		result = multierror.Append(result, err)
	}

	if err := b.subConn.Close(); err != nil {
		result = multierror.Append(result, err)
	}

	if result != nil {
		return result
	}

	// make sure to close subCh as the last step
	close(b.subCh)

	return nil
}

func (b *pubSub) Close() error {
	b.cancel()
	b.closeOnce.Do(b.close)

	b.mux.Lock()
	b.subscribers = make(map[string]map[chan *Message]struct{})
	b.isClose = true
	b.mux.Unlock()

	return nil
}

func (b *pubSub) subLoop() error {
	for {
		select {
		case m := <-b.subCh:
			mm := &Message{}
			if err := proto.Unmarshal(m.Message, mm); err != nil {
				return err
			}

			b.mux.Lock()
			for ch, _ := range b.subscribers[mm.Channel] {
				// run it in a goroutine to avoid one ch blocks another
				c := ch
				go func() {
					c <- mm
				}()
			}
			b.mux.Unlock()

		case <-b.ctx.Done():
			b.Close()
			return b.ctx.Err()
		}
	}
}

func (b *pubSub) Subscribe(name string, ch chan *Message) bool {
	b.mux.Lock()
	defer b.mux.Unlock()

	if b.isClose {
		return false
	}

	m, ok := b.subscribers[name]
	if !ok {
		m = make(map[chan *Message]struct{})
		b.subscribers[name] = m
	}

	_, ok = m[ch]
	if ok {
		return false
	}

	m[ch] = struct{}{}

	return true
}

func (b *pubSub) Unsubscribe(name string, ch chan *Message) bool {
	b.mux.Lock()
	defer b.mux.Unlock()

	if b.isClose {
		return false
	}

	m, ok := b.subscribers[name]
	if !ok {
		return false
	}

	delete(m, ch)
	if len(m) == 0 {
		delete(b.subscribers, name)
	}

	return true
}

func pubMsg(conn *radix.Pool, channel string, m *Message) error {
	bb, err := proto.Marshal(m)
	if err != nil {
		return err
	}

	return conn.Do(radix.Cmd(nil, "PUBLISH", channel, string(bb)))
}

func rpushMsg(conn *radix.Pool, channel string, m *Message) error {
	bb, err := proto.Marshal(m)
	if err != nil {
		return err
	}

	return conn.Do(radix.Cmd(nil, "RPUSH", channel, string(bb)))
}

func dialConn(opt RedisOpt) (*radix.Pool, radix.PubSubConn, error) {
	pool, err := radix.NewPool(opt.Network, opt.Address, opt.PoolSize, radix.PoolConnFunc(connFunc))
	if err != nil {
		return nil, nil, err
	}

	psc, err := radix.PersistentPubSubWithOpts(opt.Network, opt.Address, radix.PersistentPubSubConnFunc(connFunc))
	if err != nil {
		return nil, nil, err
	}

	return pool, psc, nil
}

func connFunc(network, addr string) (radix.Conn, error) {
	return radix.Dial(network, addr, radix.DialTimeout(5*time.Second))
}

func pubSubAddr(addr string) string {
	return addr + ":pubsub"
}

type addr struct{}

func (addr) Network() string { return "redisconn" }
func (addr) String() string  { return "redisconn" }
