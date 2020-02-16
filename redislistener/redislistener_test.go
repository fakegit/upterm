package redislistener

import (
	"context"
	fmt "fmt"
	"io/ioutil"
	"net"
	"net/http"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/golang/protobuf/proto"
	"github.com/google/go-cmp/cmp"
	"github.com/jingweno/upterm/memlistener"
	"github.com/rs/xid"
)

func Test_pubSub(t *testing.T) {
	t.Parallel()

	pubConn, subConn, err := dialConn(RedisOpt{Network: "tcp", Address: "localhost:6379"})
	if err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	chn := xid.New().String()
	r, err := newPubSub(ctx, chn, pubConn, subConn)
	if err != nil {
		t.Fatal(err)
	}

	ch := make(chan *Message)
	if want, got := true, r.Subscribe("1234", ch); want != got {
		t.Fatal("Subscribe should return true")
	}

	m := &Message{
		Kind:    Message_DATA,
		Channel: "1234",
		Body:    []byte("test"),
	}

	if err := r.Publish(m); err != nil {
		t.Fatal(err)
	}

	if diff := cmp.Diff(m, <-ch, cmp.Comparer(proto.Equal)); diff != "" {
		t.Fatal(diff)
	}

	if want, got := true, r.Unsubscribe("1234", ch); want != got {
		t.Fatal("Unsubscribe should return true")
	}

	select {
	case m := <-ch:
		t.Fatalf("should not receive msg after unsubscribe: %v", m)
	case <-time.After(time.Second):
		// wait a bit to make sure ch doesn't receive msg
	}

	// Test closing of pubsub
	if err := r.Close(); err != nil {
		t.Fatal(err)
	}
	if want, got := false, r.Subscribe("1234", ch); want != got {
		t.Fatal("Subscribe should return false due to pubsub is closed")
	}
	if want, got := false, r.Unsubscribe("1234", ch); want != got {
		t.Fatal("Unsubscribe should return false due to pubsub is closed")
	}
	if err := r.Close(); err != nil {
		t.Fatal(err)
	}
}

func Test_conn(t *testing.T) {
	t.Parallel()

	pubConn, subConn, err := dialConn(RedisOpt{Network: "tcp", Address: "localhost:6379"})
	if err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	chn := xid.New().String()
	pubSub, err := newPubSub(ctx, chn, pubConn, subConn)
	if err != nil {
		t.Fatal(err)
	}
	defer pubSub.Close()

	conn1 := newConn(ctx, "1234", "read", "write", pubSub)
	defer conn1.Close()

	conn2 := newConn(ctx, "2345", "write", "read", pubSub)
	defer conn2.Close()

	_, err = conn1.Write([]byte("hello"))
	if err != nil {
		t.Fatal(err)
	}

	b := make([]byte, 5)
	_, err = conn2.Read(b)
	if err != nil {
		t.Fatal(err)
	}

	if diff := cmp.Diff("hello", string(b)); diff != "" {
		t.Fatalf(diff)
	}

	_, err = conn2.Write([]byte("hello"))
	if err != nil {
		t.Fatal(err)
	}

	b = make([]byte, 5)
	_, err = conn1.Read(b)
	if err != nil {
		t.Fatal(err)
	}

	if diff := cmp.Diff("hello", string(b)); diff != "" {
		t.Fatalf(diff)
	}
}

func Test_Listener(t *testing.T) {
	t.Parallel()

	opt := RedisOpt{Network: "tcp", Address: "localhost:6379"}
	addr := xid.New().String()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	ln, err := NewListener(ctx, addr, opt)
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()

	dialer, err := NewDialer(ctx, addr, opt)
	if err != nil {
		t.Fatal(err)
	}
	defer dialer.Close()

	n := 100

	for i := 0; i < n; i++ {
		go func(t *testing.T) {
			c, err := dialer.Dial()
			if err != nil {
				t.Fatal(err)
			}

			_, err = c.Write([]byte("world"))
			if err != nil {
				t.Fatal(err)
			}
		}(t)
	}

	for i := 0; i < n; i++ {
		c, err := ln.Accept()
		if err != nil {
			t.Fatal(err)
		}

		b := make([]byte, 5)
		_, err = c.Read(b)
		if err != nil {
			t.Fatal(err)
		}

		if diff := cmp.Diff("world", string(b)); diff != "" {
			t.Fatalf(diff)
		}
	}
}

func Test_HTTP(t *testing.T) {
	t.Parallel()

	opt := RedisOpt{Network: "tcp", Address: "localhost:6379"}
	addr := xid.New().String()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	numOfSrv := 2
	var srvWg sync.WaitGroup
	for i := 0; i < numOfSrv; i++ {
		srvWg.Add(1)

		ln, err := NewListener(ctx, addr, opt)
		if err != nil {
			t.Fatal(err)
		}
		defer ln.Close()

		srv := &http.Server{
			Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				// response with path
				fmt.Fprintf(w, r.URL.Path)
			}),
		}
		defer srv.Close()

		go func() {
			srvWg.Done()
			_ = srv.Serve(ln)
		}()
	}

	srvWg.Wait()

	dialer, err := NewDialer(ctx, addr, opt)
	if err != nil {
		t.Fatal(err)
	}
	defer dialer.Close()

	numOfClients := 1000
	var count uint64
	var wg sync.WaitGroup

	for i := 0; i < numOfClients; i++ {
		wg.Add(1)

		go func(t *testing.T, i int) {
			defer wg.Done()

			tr := &http.Transport{
				DialContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
					return dialer.DialContext(ctx)
				},
			}

			client := &http.Client{Transport: tr}
			resp, err := client.Get(fmt.Sprintf("http://%s/%d", addr, i))
			if err != nil {
				t.Fatal(err)
			}

			b, err := ioutil.ReadAll(resp.Body)
			if err != nil {
				t.Fatal(err)
			}

			if string(b) == "/"+strconv.Itoa(i) {
				atomic.AddUint64(&count, 1)
			}
		}(t, i)
	}

	wg.Wait()

	if want, got := numOfClients, int(count); want != got {
		t.Fatalf("want=%d, got=%d", want, got)
	}
}

func Benchmark_MemoryListener(b *testing.B) {
	addr := xid.New().String()
	ln, err := memlistener.Listen("mem", addr)
	if err != nil {
		b.Fatal(err)
	}
	defer ln.Close()

	var srvWg sync.WaitGroup
	srvWg.Add(1)

	srv := &http.Server{
		Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			fmt.Fprintf(w, "hello")
		}),
	}
	defer srv.Close()

	go func() {
		srvWg.Done()
		_ = srv.Serve(ln)
	}()

	srvWg.Wait()

	tr := &http.Transport{
		DialContext: func(ctx context.Context, network, address string) (net.Conn, error) {
			return memlistener.Dial("mem", addr)
		},
	}
	client := &http.Client{Transport: tr}

	b.RunParallel(func(pb *testing.PB) {
		for pb.Next() {
			resp, err := client.Get(fmt.Sprintf("http://%s", addr))
			if err != nil {
				b.Fatal(err)
			}

			bb, err := ioutil.ReadAll(resp.Body)
			if err != nil {
				b.Fatal(err)
			}

			if string(bb) != "hello" {
				b.Fatalf("unexpected response %s", bb)
			}
		}
	})
}

func Benchmark_RedisListener(b *testing.B) {
	opt := RedisOpt{Network: "tcp", Address: "localhost:6379"}
	addr := xid.New().String()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	var srvWg sync.WaitGroup
	srvWg.Add(1)

	ln, err := NewListener(ctx, addr, opt)
	if err != nil {
		b.Fatal(err)
	}
	defer ln.Close()

	srv := &http.Server{
		Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			fmt.Fprintf(w, "hello")
		}),
	}
	defer srv.Close()

	go func() {
		srvWg.Done()
		_ = srv.Serve(ln)
	}()

	srvWg.Wait()

	dialer, err := NewDialer(ctx, addr, opt)
	if err != nil {
		b.Fatal(err)
	}
	defer dialer.Close()

	tr := &http.Transport{
		DialContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
			return dialer.Dial()
		},
	}
	client := &http.Client{Transport: tr}

	b.RunParallel(func(pb *testing.PB) {
		for pb.Next() {
			resp, err := client.Get(fmt.Sprintf("http://%s", addr))
			if err != nil {
				b.Fatal(err)
			}

			bb, err := ioutil.ReadAll(resp.Body)
			if err != nil {
				b.Fatal(err)
			}

			if string(bb) != "hello" {
				b.Fatalf("unexpected response %s", bb)
			}
		}
	})
}
