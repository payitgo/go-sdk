package payitgo

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/coder/websocket"
)

// fakeRealtime is payitgo's socket server as the SDK sees it: it opens with
// a short ping interval, checks the key in the CONNECT packet and records
// each CONNECT's auth; serve then plays the rest of each connection.
type fakeRealtime struct {
	mu    sync.Mutex
	auths []map[string]string
	pongs int
	serve func(n int, conn *websocket.Conn, auth map[string]string)
}

func (f *fakeRealtime) handler(t *testing.T) http.HandlerFunc {
	n := 0
	return func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/socket.io/" || r.URL.Query().Get("EIO") != "4" || r.URL.Query().Get("transport") != "websocket" {
			http.NotFound(w, r)
			return
		}
		conn, err := websocket.Accept(w, r, nil)
		if err != nil {
			return
		}
		defer conn.CloseNow() //nolint:errcheck
		ctx := r.Context()
		_ = conn.Write(ctx, websocket.MessageText, []byte(`0{"sid":"e1","upgrades":[],"pingInterval":100,"pingTimeout":200,"maxPayload":1000000}`))
		_, msg, err := conn.Read(ctx)
		if err != nil {
			return
		}
		data, ok := strings.CutPrefix(string(msg), "40/v1,")
		auth := map[string]string{}
		if !ok || json.Unmarshal([]byte(data), &auth) != nil {
			t.Errorf("CONNECT packet %q", msg)
			return
		}
		f.mu.Lock()
		f.auths = append(f.auths, auth)
		n++
		count := n
		f.mu.Unlock()
		// Pongs are counted in the background while serve plays
		go func() {
			for {
				_, msg, err := conn.Read(ctx)
				if err != nil {
					return
				}
				if string(msg) == "3" {
					f.mu.Lock()
					f.pongs++
					f.mu.Unlock()
				}
			}
		}()
		f.serve(count, conn, auth)
	}
}

func send(conn *websocket.Conn, packet string) {
	_ = conn.Write(context.Background(), websocket.MessageText, []byte(packet))
}

// Listen delivers each payment update, in order, answers payitgo's pings,
// resumes a dropped session with its pid and the last update's offset, and
// reports a refused key with payitgo's code.
func TestRealtimeListen(t *testing.T) {
	f := &fakeRealtime{}
	f.serve = func(n int, conn *websocket.Conn, auth map[string]string) {
		switch {
		case auth["key"] != "sk_good":
			send(conn, `44/v1,{"message":"unauthorized","data":{"code":"invalid_key","message":"invalid api key"}}`)
		case n == 1:
			send(conn, `40/v1,{"sid":"s1","pid":"p1"}`)
			send(conn, `2`) // ping
			send(conn, `42/v1,["payment:update",{"type":"payment.detecting","id":"pay1","payment":{"id":"pay1","status":"detecting","confirmations":2,"confirmations_required":20}},"off-1"]`)
			send(conn, `42/v1,["something:else",{}]`)
			send(conn, `42/v1,["payment:update",{"type":"payment.confirmations","id":"pay1","payment":{"id":"pay1","status":"detecting","confirmations":9,"confirmations_required":20}},"off-2"]`)
			time.Sleep(300 * time.Millisecond)
			// then the connection drops
		default:
			send(conn, `40/v1,{"sid":"s2","pid":"p1"}`) // the same session: resumed
			send(conn, `42/v1,["payment:update",{"type":"payment.confirmed","id":"pay1","payment":{"id":"pay1","status":"confirmed"}},"off-3"]`)
			<-time.After(5 * time.Second)
		}
	}
	srv := httptest.NewServer(f.handler(t))
	defer srv.Close()

	client, err := NewClient("sk_good", WithBaseURL(srv.URL))
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	var (
		mu       sync.Mutex
		got      []string
		connects []bool
		drops    int
	)
	done := make(chan struct{})
	go func() {
		_ = client.Realtime.Listen(ctx, Listener{
			Payment: func(u *PaymentUpdate) {
				mu.Lock()
				defer mu.Unlock()
				got = append(got, u.Type+" "+u.Payment.Status)
				if u.Payment.Status == "confirmed" {
					close(done)
				}
			},
			Connected:    func(resumed bool) { mu.Lock(); connects = append(connects, resumed); mu.Unlock() },
			Disconnected: func(error) { mu.Lock(); drops++; mu.Unlock() },
		})
	}()
	select {
	case <-done:
	case <-ctx.Done():
		t.Fatal("the confirmed update never arrived")
	}
	cancel()
	mu.Lock()
	want := []string{"payment.detecting detecting", "payment.confirmations detecting", "payment.confirmed confirmed"}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Errorf("updates %q, want %q", got, want)
	}
	if len(connects) != 2 || connects[0] || !connects[1] || drops != 1 {
		t.Errorf("connected %v with %d drops, want a first connection then a resumed one after one drop", connects, drops)
	}
	mu.Unlock()
	f.mu.Lock()
	if len(f.auths) != 2 || f.auths[0]["pid"] != "" || f.auths[1]["pid"] != "p1" || f.auths[1]["offset"] != "off-2" {
		t.Errorf("CONNECT auths %v, want the second to resume p1 after off-2", f.auths)
	}
	if f.pongs == 0 {
		t.Error("payitgo's ping was not answered")
	}
	f.mu.Unlock()

	bad, _ := NewClient("sk_bad", WithBaseURL(srv.URL))
	bctx, bcancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer bcancel()
	refused := make(chan error, 1)
	go func() {
		_ = bad.Realtime.Listen(bctx, Listener{Disconnected: func(err error) {
			select {
			case refused <- err:
			default:
			}
		}})
	}()
	select {
	case err := <-refused:
		var re *RealtimeError
		if !errors.As(err, &re) || re.Code != "invalid_key" || re.Message != "invalid api key" {
			t.Errorf("a wrong key: %v, want invalid_key", err)
		}
	case <-bctx.Done():
		t.Error("a wrong key was not reported")
	}
}
