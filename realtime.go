package payitgo

import (
	"context"
	"encoding/json"
	"errors"
	"math/rand/v2"
	"net/http"
	"strings"
	"time"

	"github.com/coder/websocket"
)

// RealtimeService follows the key's payments as they change, over one
// connection to payitgo (socket.io on a WebSocket): each change arrives the
// moment payitgo knows it, a deposit seen, every confirmation, the final
// status, with nothing to poll. Webhooks still carry the same news.
//
//	err := client.Realtime.Listen(ctx, payitgo.Listener{
//		Payment: func(u *payitgo.PaymentUpdate) { log.Println(u.Payment.ID, u.Payment.Status) },
//	})
type RealtimeService struct{ c *Client }

// PaymentUpdate is one change to a payment, pushed as it happens.
type PaymentUpdate struct {
	// Type is what happened: a webhook event's type (payment.confirmed,
	// payment.detecting…) when one was sent, else payment.created,
	// payment.coin_chosen, payment.confirmations (a deposit collected more),
	// payment.deposit or payment.simulation.
	Type string `json:"type"`
	// Payment is the whole payment after the change, as Payments.Get
	// returns it.
	Payment *Payment `json:"payment"`
}

// RealtimeError is why payitgo refused or ended the realtime connection.
// Code is invalid_key, forbidden (a suspended account, live mode off),
// ip_not_allowed, key_revoked, key_changed or suspended; it is "" for a
// network error, whose Message says what happened.
type RealtimeError struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

func (e *RealtimeError) Error() string {
	if e.Code == "" {
		return "payitgo realtime: " + e.Message
	}
	return "payitgo realtime: " + e.Message + " (" + e.Code + ")"
}

// Listener is what Listen does with what arrives. Each function is called
// from Listen's goroutine, one call at a time and in order: return quickly
// (hand slow work to another goroutine), or the connection falls behind.
// Any of them may be nil.
type Listener struct {
	// Payment is called with every change to the key's payments: those of
	// its mode, and only its store's for a store's key.
	Payment func(*PaymentUpdate)
	// Connected is called each time the connection is up. resumed reports
	// that the updates sent while it was down were delivered again: when
	// it is false after a drop, read again (Payments.Get) the payments you
	// follow, since an update may have been missed.
	Connected func(resumed bool)
	// Disconnected is called when the connection drops or is refused, with
	// why (a *RealtimeError for payitgo's refusals). Listen connects again
	// after a pause.
	Disconnected func(err error)
}

// realtimeNamespace is the socket.io namespace servers connect to.
const realtimeNamespace = "/v1"

// Listen connects to payitgo with the client's key and delivers the key's
// payment updates to l until ctx ends, then returns ctx's error. It never
// gives up on its own: a dropped or refused connection is tried again after
// a pause growing to a minute, and a drop of up to two minutes is resumed
// without missing an update (Listener.Connected says when one may have
// been). Call it in its own goroutine.
func (s *RealtimeService) Listen(ctx context.Context, l Listener) error {
	var st realtimeSession
	pause := time.Second
	for {
		connected, err := s.session(ctx, &st, l)
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if connected {
			pause = time.Second
		}
		if l.Disconnected != nil && err != nil {
			l.Disconnected(err)
		}
		wait := pause/2 + time.Duration(rand.Int64N(int64(pause))) // 0.5 to 1.5 times the pause
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(wait):
		}
		pause = min(pause*2, time.Minute)
	}
}

// realtimeSession is what a dropped connection is resumed with: the
// session payitgo gave (pid) and the last update received (offset).
type realtimeSession struct {
	pid, offset string
}

// realtimeURL is the socket.io endpoint at the client's address.
func (s *RealtimeService) realtimeURL() string {
	return s.c.baseURL + "/socket.io/?EIO=4&transport=websocket"
}

// session runs one connection until it ends; connected reports that
// payitgo accepted it.
func (s *RealtimeService) session(ctx context.Context, st *realtimeSession, l Listener) (connected bool, err error) {
	dial, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	conn, _, err := websocket.Dial(dial, s.realtimeURL(), &websocket.DialOptions{
		HTTPClient: s.c.http,
		HTTPHeader: http.Header{"User-Agent": {"payitgo-go/" + Version}},
	})
	if err != nil {
		return false, &RealtimeError{Message: err.Error()}
	}
	defer conn.CloseNow() //nolint:errcheck
	conn.SetReadLimit(1 << 20)

	// Engine.IO's open packet: how often payitgo pings
	open, err := readPacket(ctx, conn, 30*time.Second)
	if err != nil {
		return false, err
	}
	var hs struct {
		PingInterval int64 `json:"pingInterval"`
		PingTimeout  int64 `json:"pingTimeout"`
		MaxPayload   int64 `json:"maxPayload"`
	}
	if !strings.HasPrefix(open, "0") || json.Unmarshal([]byte(open[1:]), &hs) != nil {
		return false, &RealtimeError{Message: "unexpected opening: " + truncate(open)}
	}
	if hs.MaxPayload > 1<<20 {
		conn.SetReadLimit(hs.MaxPayload)
	}
	// No ping for this long: payitgo is gone
	silence := time.Duration(hs.PingInterval+hs.PingTimeout) * time.Millisecond
	if silence <= 0 {
		silence = time.Minute
	}

	auth := map[string]string{"key": s.c.apiKey}
	if st.pid != "" {
		auth["pid"], auth["offset"] = st.pid, st.offset
	}
	body, _ := json.Marshal(auth)
	if err := conn.Write(ctx, websocket.MessageText, []byte("40"+realtimeNamespace+","+string(body))); err != nil {
		return false, &RealtimeError{Message: err.Error()}
	}

	prefix := "4"         // a socket.io packet on the namespace: 4<type>/v1,<data>
	var why *RealtimeError // what payitgo said before ending the session (auth:error)
	for {
		packet, err := readPacket(ctx, conn, silence)
		if err != nil {
			return connected, err
		}
		switch {
		case packet == "2": // ping
			if err := conn.Write(ctx, websocket.MessageText, []byte("3")); err != nil {
				return connected, &RealtimeError{Message: err.Error()}
			}
		case packet == "1": // payitgo closes the transport
			return connected, &RealtimeError{Message: "payitgo closed the connection"}
		case !strings.HasPrefix(packet, prefix) || len(packet) < 2:
			// noop and anything else of the transport's
		default:
			kind, data := packet[1], strings.TrimPrefix(packet[2:], realtimeNamespace+",")
			if data == packet[2:] && realtimeNamespace != "/" {
				continue // another namespace's
			}
			switch kind {
			case '0': // connected
				var ok struct {
					PID string `json:"pid"`
				}
				_ = json.Unmarshal([]byte(data), &ok)
				resumed := st.pid != "" && ok.PID == st.pid
				if !resumed {
					st.offset = ""
				}
				st.pid = ok.PID
				connected = true
				if l.Connected != nil {
					l.Connected(resumed)
				}
			case '4': // refused
				var refused struct {
					Message string        `json:"message"`
					Data    RealtimeError `json:"data"`
				}
				_ = json.Unmarshal([]byte(data), &refused)
				st.pid, st.offset = "", ""
				if refused.Data.Message == "" {
					refused.Data.Message = refused.Message
				}
				return false, &refused.Data
			case '1': // payitgo ended the session: it cannot be resumed
				st.pid, st.offset = "", ""
				if why != nil {
					return connected, why
				}
				return connected, &RealtimeError{Message: "payitgo ended the session"}
			case '2': // an event
				if e := s.event(st, data, l); e != nil {
					why = e
				}
			}
		}
	}
}

// event delivers one event: [name, data…, offset?]. An auth:error is
// returned: why payitgo is about to end the session.
func (s *RealtimeService) event(st *realtimeSession, data string, l Listener) *RealtimeError {
	if i := strings.IndexByte(data, '['); i > 0 {
		data = data[i:] // an acknowledgement id first: none is asked for
	}
	var args []json.RawMessage
	if json.Unmarshal([]byte(data), &args) != nil || len(args) == 0 {
		return nil
	}
	if st.pid != "" && len(args) > 1 {
		var offset string
		if json.Unmarshal(args[len(args)-1], &offset) == nil {
			st.offset = offset // payitgo numbers each update, to resume after it
		}
	}
	var name string
	_ = json.Unmarshal(args[0], &name)
	switch name {
	case "payment:update":
		if len(args) < 2 || l.Payment == nil {
			return nil
		}
		var u PaymentUpdate
		if json.Unmarshal(args[1], &u) == nil && u.Payment != nil {
			l.Payment(&u)
		}
	case "auth:error":
		var why RealtimeError
		if len(args) > 1 && json.Unmarshal(args[1], &why) == nil && why.Code != "" {
			return &why
		}
	}
	return nil
}

// readPacket is the next Engine.IO packet, waiting at most wait.
func readPacket(ctx context.Context, conn *websocket.Conn, wait time.Duration) (string, error) {
	read, cancel := context.WithTimeout(ctx, wait)
	defer cancel()
	_, msg, err := conn.Read(read)
	switch {
	case err == nil:
		return string(msg), nil
	case errors.Is(read.Err(), context.DeadlineExceeded) && ctx.Err() == nil:
		return "", &RealtimeError{Message: "payitgo went quiet: no ping for " + wait.String()}
	default:
		return "", &RealtimeError{Message: err.Error()}
	}
}

func truncate(s string) string {
	if len(s) > 80 {
		return s[:80] + "…"
	}
	return s
}
