package main

import (
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/binary"
	"errors"
	"fmt"
	"log"
	"net"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/go-mclib/client"
	"github.com/go-mclib/data/packet_ids"
	"github.com/go-mclib/data/packets"
	jp "github.com/go-mclib/protocol/java_protocol"
)

const (
	voiceCompatibility      = 20
	voiceMagic              = 0xff
	voiceMic                = 1
	voiceAuthenticate       = 5
	voiceAuthenticateAck    = 6
	voicePing               = 7
	voiceKeepAlive          = 8
	voiceConnectionCheck    = 9
	voiceConnectionCheckAck = 10
	voiceMaxPayload         = 2048
	voiceMaxOpus            = 1275
	voiceHandshakeTimeout   = 5 * time.Second
	voiceRetryInterval      = time.Second
)

var errVoiceClosed = errors.New("voice chat transport closed")

// voiceChat owns one Minecraft connection's secret and UDP session. A server
// issues its secret only once, so cancelling a caller must not discard it.
type voiceChat struct {
	mu         sync.Mutex
	lifecycle  sync.Mutex
	client     *client.Client
	connection *voiceConnection
}

type voiceConnection struct {
	ctx         context.Context
	cancel      context.CancelFunc
	host        string
	queue       chan jp.Packet
	requested   bool
	secretReady chan struct{}
	secret      *voiceSecret
	secretErr   error
	attempt     *voiceAttempt
	session     *voiceSession
}

type voiceAttempt struct {
	done    chan struct{}
	session *voiceSession
	err     error
}

type voiceSecret struct {
	key       [16]byte
	player    [16]byte
	port      int
	mtu       int
	keepAlive time.Duration
	host      string
}

var _ client.Module = (*voiceChat)(nil)

func newVoiceChat() *voiceChat  { return &voiceChat{} }
func (*voiceChat) Name() string { return "voicechat" }

func (v *voiceChat) Init(c *client.Client) {
	v.client = c
	c.OnPlay(v.onPlay)
	c.OnTransfer(v.close)
	c.OnDisconnect(v.close)
}

func (v *voiceChat) onPlay() {
	v.lifecycle.Lock()
	defer v.lifecycle.Unlock()
	v.closeConnection()
	host, _ := v.client.ResolvedAddr()
	if conn := v.client.Conn(); conn != nil && conn.RemoteAddr() != nil {
		if remote, _, err := net.SplitHostPort(conn.RemoteAddr().String()); err == nil {
			host = remote
		}
	}
	ctx, cancel := context.WithCancel(context.Background())
	v.mu.Lock()
	v.connection = &voiceConnection{
		ctx: ctx, cancel: cancel, host: host, queue: v.client.OutgoingPacketQueue,
		secretReady: make(chan struct{}),
	}
	v.mu.Unlock()
}

func (v *voiceChat) Reset() { v.close() }

func (v *voiceChat) close() {
	v.lifecycle.Lock()
	defer v.lifecycle.Unlock()
	v.closeConnection()
}

func (v *voiceChat) closeConnection() {
	v.mu.Lock()
	c := v.connection
	v.connection = nil
	var attempt *voiceAttempt
	if c != nil {
		c.cancel()
		attempt = c.attempt
	}
	v.mu.Unlock()
	if c == nil {
		return
	}
	// The attempt owns a not-yet-published UDP socket; cancellation closes it.
	if attempt != nil {
		<-attempt.done
	}
	v.mu.Lock()
	session := c.session
	v.mu.Unlock()
	if session != nil {
		session.close()
	}
}

func (v *voiceChat) HandlePacket(pkt *jp.WirePacket) {
	if v.client.State() != jp.StatePlay || pkt.PacketID != packet_ids.S2CCustomPayloadPlayID {
		return
	}
	// Decode the channel without letting a malicious length allocate memory.
	data := []byte(pkt.Data)
	n, used, ok := voiceReadLength(data, 256)
	if !ok || n > len(data)-used || string(data[used:used+n]) != "voicechat:secret" {
		return
	}
	secret, err := parseVoiceSecret(data[used+n:])
	v.mu.Lock()
	defer v.mu.Unlock()
	c := v.connection
	if c == nil || !c.requested || c.secret != nil || c.secretErr != nil {
		return
	}
	c.secret, c.secretErr = secret, err
	close(c.secretReady)
}

func (v *voiceChat) connect(ctx context.Context) (*voiceSession, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	v.mu.Lock()
	c := v.connection
	if c == nil {
		v.mu.Unlock()
		return nil, errors.New("voice chat unavailable: Minecraft is not in play")
	}
	session := c.session
	v.mu.Unlock()
	if session != nil && session.available() == nil {
		if c.ctx.Err() != nil {
			return nil, errVoiceClosed
		}
		return session, nil
	}
	v.mu.Lock()
	if v.connection != c {
		v.mu.Unlock()
		return nil, errVoiceClosed
	}
	a := c.attempt
	if a == nil {
		a = &voiceAttempt{done: make(chan struct{})}
		c.attempt = a
		go v.negotiate(c, a)
	}
	v.mu.Unlock()
	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-c.ctx.Done():
		return nil, errVoiceClosed
	case <-a.done:
		if c.ctx.Err() != nil {
			return nil, errVoiceClosed
		}
		return a.session, a.err
	}
}

func (v *voiceChat) negotiate(c *voiceConnection, a *voiceAttempt) {
	ctx, cancel := context.WithTimeout(c.ctx, voiceHandshakeTimeout)
	defer cancel()
	v.mu.Lock()
	old := c.session
	c.session = nil
	v.mu.Unlock()
	if old != nil {
		old.close()
	}
	s, err := v.openSession(ctx, c)
	if c.ctx.Err() != nil && err == nil {
		err = errVoiceClosed
	}
	if err != nil && s != nil {
		s.close()
	}
	if err != nil && !errors.Is(err, context.Canceled) && !errors.Is(err, errVoiceClosed) {
		log.Printf("voice chat unavailable: %v", err)
	}
	v.mu.Lock()
	if err == nil {
		c.session = s
		a.session = s
	}
	a.err = err
	c.attempt = nil
	close(a.done)
	v.mu.Unlock()
}

func (v *voiceChat) openSession(ctx context.Context, c *voiceConnection) (*voiceSession, error) {
	v.mu.Lock()
	request := !c.requested
	c.requested = true
	v.mu.Unlock()
	if request {
		// SendPacket itself cannot be cancelled while its queue is full.
		select {
		case c.queue <- &packets.C2SCustomPayloadPlay{
			Channel: "voicechat:request_secret", Data: []byte{0, 0, 0, voiceCompatibility},
		}:
		case <-ctx.Done():
			v.mu.Lock()
			c.requested = false
			v.mu.Unlock()
			return nil, fmt.Errorf("requesting voice secret: %w", ctx.Err())
		}
	}
	select {
	case <-ctx.Done():
		return nil, fmt.Errorf("no voice secret (missing/incompatible Simple Voice Chat server): %w", ctx.Err())
	case <-c.secretReady:
	}
	if c.secretErr != nil {
		return nil, c.secretErr
	}
	endpoint, err := voiceEndpoint(c.secret.host, c.host, c.secret.port)
	if err != nil {
		return nil, err
	}
	s, err := dialVoiceSession(ctx, endpoint, c.secret)
	if err != nil {
		return nil, err
	}
	return s, nil
}

// The fixed prefix is key | port | UUID | codec | MTU | distance | keepalive |
// groups. Strings use Minecraft VarInt byte lengths, not Go's signed varints.
func parseVoiceSecret(data []byte) (*voiceSecret, error) {
	const fixed = 54
	if len(data) < fixed+2 || len(data) > fixed+3+32767*3+1 {
		return nil, errors.New("invalid voice secret size")
	}
	n, used, ok := voiceReadLength(data[fixed:], 32767*3)
	if !ok || fixed+used+n+1 != len(data) || !utf8.Valid(data[fixed+used:fixed+used+n]) {
		return nil, errors.New("invalid voice host encoding")
	}
	s := &voiceSecret{
		port:      int(int32(binary.BigEndian.Uint32(data[16:20]))),
		mtu:       int(int32(binary.BigEndian.Uint32(data[37:41]))),
		keepAlive: time.Duration(int32(binary.BigEndian.Uint32(data[49:53]))) * time.Millisecond,
		host:      string(data[fixed+used : fixed+used+n]),
	}
	copy(s.key[:], data[:16])
	copy(s.player[:], data[20:36])
	// MTU is the reference encoder's maximum Opus payload, not the UDP size.
	s.mtu = min(max(s.mtu, 256), voiceMaxOpus)
	s.keepAlive = max(s.keepAlive, 100*time.Millisecond)
	return s, nil
}

func voiceEndpoint(override, gameHost string, port int) (string, error) {
	host := gameHost
	if override != "" {
		if n, err := strconv.ParseInt(override, 10, 32); err == nil {
			if n > 0 && n <= 65535 {
				port = int(n)
			}
		} else {
			u, err := url.Parse("voicechat://" + override)
			if err != nil || u.Hostname() == "" || u.User != nil || u.Path != "" || u.RawQuery != "" || u.Fragment != "" {
				return "", errors.New("invalid voice server endpoint override")
			}
			host = u.Hostname()
			if u.Port() != "" {
				n, err := strconv.Atoi(u.Port())
				if err != nil || n < 0 || n > 65535 {
					return "", errors.New("invalid voice server port override")
				}
				if n > 0 {
					port = n
				}
			}
		}
	}
	if host == "" || port < 1 || port > 65535 || strings.ContainsAny(host, "/?#@") {
		return "", errors.New("invalid voice server endpoint")
	}
	return net.JoinHostPort(host, strconv.Itoa(port)), nil
}

func voiceReadLength(data []byte, limit int) (int, int, bool) {
	var value uint32
	for i := 0; i < 5 && i < len(data); i++ {
		b := data[i]
		if i == 4 && b > 7 {
			return 0, 0, false
		}
		value |= uint32(b&127) << (7 * i)
		if b&128 == 0 {
			return int(value), i + 1, value <= uint32(limit)
		}
	}
	return 0, 0, false
}

// A single receive goroutine drives handshake retries and liveness deadlines.
// All writes share fixed buffers under mu; the caller alone paces audio.
type voiceSession struct {
	mu            sync.Mutex
	conn          net.Conn
	crypto        cipher.AEAD
	secret        *voiceSecret
	ready         chan struct{}
	done          chan struct{}
	state         byte
	err           error
	lastKeepAlive time.Time
	sequence      uint64
	plain         [voiceMaxPayload]byte
	frame         [voiceMaxPayload + 32]byte
}

func dialVoiceSession(ctx context.Context, endpoint string, secret *voiceSecret) (*voiceSession, error) {
	block, err := aes.NewCipher(secret.key[:])
	if err != nil {
		return nil, err
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		return nil, err
	}
	conn, err := (&net.Dialer{}).DialContext(ctx, "udp", endpoint)
	if err != nil {
		return nil, fmt.Errorf("opening voice UDP socket: %w", err)
	}
	s := &voiceSession{
		conn: conn, crypto: aead, secret: secret, ready: make(chan struct{}),
		done: make(chan struct{}), state: voiceAuthenticate,
	}
	cancelled := make(chan struct{})
	stop := context.AfterFunc(ctx, func() {
		s.fail(ctx.Err())
		close(cancelled)
	})
	go s.receiveLoop()
	select {
	case <-ctx.Done():
		s.fail(ctx.Err())
	case <-s.ready:
	}
	if !stop() {
		<-cancelled
	}
	if err := s.available(); err != nil {
		s.close()
		if ctx.Err() != nil {
			err = ctx.Err()
		}
		return nil, fmt.Errorf("voice UDP handshake: %w", err)
	}
	log.Printf("voice chat connected to %s", conn.RemoteAddr())
	return s, nil
}

func (s *voiceSession) available() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.availableLocked()
}

func (s *voiceSession) availableLocked() error {
	if s.err != nil {
		return s.err
	}
	if s.state != voiceConnectionCheckAck {
		return errors.New("voice chat not authenticated")
	}
	if time.Since(s.lastKeepAlive) >= s.secret.keepAlive*10 {
		return errors.New("voice chat keepalive timed out")
	}
	return nil
}

func (s *voiceSession) fail(err error) {
	// Closing before locking interrupts an in-flight UDP write immediately.
	_ = s.conn.Close()
	s.mu.Lock()
	if s.err == nil {
		s.err = err
		if s.state != voiceConnectionCheckAck {
			close(s.ready)
		}
	}
	s.mu.Unlock()
}

func (s *voiceSession) close() {
	s.fail(errVoiceClosed)
	<-s.done
}

func (s *voiceSession) sendAudio(opus []byte) error {
	s.mu.Lock()
	if err := s.availableLocked(); err != nil {
		s.mu.Unlock()
		return err
	}
	if len(opus) > s.secret.mtu {
		s.mu.Unlock()
		return fmt.Errorf("Opus frame is %d bytes; voice server permits %d", len(opus), s.secret.mtu)
	}
	body := binary.AppendUvarint(s.plain[1:1], uint64(len(opus)))
	body = append(body, opus...)
	body = binary.BigEndian.AppendUint64(body, s.sequence)
	body = append(body, 0) // whispering=false
	err := s.sendLocked(voiceMic, body)
	if err == nil {
		s.sequence++
	}
	s.mu.Unlock()
	if err != nil {
		s.fail(err)
	}
	return err
}

func (s *voiceSession) send(packet byte, body []byte) error {
	s.mu.Lock()
	err := s.sendLocked(packet, body)
	s.mu.Unlock()
	return err
}

func (s *voiceSession) sendLocked(packet byte, body []byte) error {
	if s.err != nil {
		return s.err
	}
	if len(body)+1+12+16 > voiceMaxPayload {
		return errors.New("voice payload exceeds protocol limit")
	}
	s.plain[0] = packet
	copy(s.plain[1:], body)
	frame := s.frame[:1]
	frame[0] = voiceMagic
	frame = append(frame, s.secret.player[:]...)
	frame = binary.AppendUvarint(frame, uint64(12+len(body)+1+16))
	nonceAt := len(frame)
	frame = frame[:nonceAt+12]
	if _, err := rand.Read(frame[nonceAt:]); err != nil {
		return err
	}
	frame = s.crypto.Seal(frame, frame[nonceAt:], s.plain[:len(body)+1], nil)
	if err := s.conn.SetWriteDeadline(time.Now().Add(250 * time.Millisecond)); err != nil {
		return err
	}
	_, err := s.conn.Write(frame)
	return err
}

func (s *voiceSession) receiveLoop() {
	defer close(s.done)
	var buffer [4096]byte
	var auth [32]byte
	copy(auth[:16], s.secret.player[:])
	copy(auth[16:], s.secret.key[:])
	var nextRetry time.Time
	for {
		s.mu.Lock()
		state, failed, last := s.state, s.err, s.lastKeepAlive
		s.mu.Unlock()
		if failed != nil {
			return
		}
		now := time.Now()
		deadline := nextRetry
		if state == voiceConnectionCheckAck {
			deadline = last.Add(s.secret.keepAlive * 10)
			if !now.Before(deadline) {
				s.fail(errors.New("voice chat keepalive timed out"))
				return
			}
		} else if !now.Before(nextRetry) {
			var body []byte
			if state == voiceAuthenticate {
				body = auth[:]
			}
			if err := s.send(state, body); err != nil {
				s.fail(err)
				return
			}
			nextRetry = now.Add(voiceRetryInterval)
			deadline = nextRetry
		}
		if err := s.conn.SetReadDeadline(deadline); err != nil {
			s.fail(err)
			return
		}
		n, err := s.conn.Read(buffer[:])
		if err != nil {
			if timeout, ok := err.(net.Error); ok && timeout.Timeout() {
				continue
			}
			s.fail(err)
			return
		}
		payload := s.parseInbound(buffer[:n])
		if len(payload) == 0 {
			continue
		}
		packet, body := payload[0], payload[1:]
		s.mu.Lock()
		if s.err != nil {
			s.mu.Unlock()
			return
		}
		// Bodyless control packets may carry authenticated padding. Like the
		// Java client, ignore it instead of rejecting a valid acknowledgement.
		switch {
		case packet == voiceAuthenticateAck && s.state == voiceAuthenticate:
			s.state = voiceConnectionCheck
			nextRetry = time.Time{}
		case packet == voiceConnectionCheckAck && s.state == voiceConnectionCheck:
			s.state = voiceConnectionCheckAck
			s.lastKeepAlive = time.Now()
			close(s.ready)
		case packet == voiceKeepAlive:
			s.lastKeepAlive = time.Now()
			err = s.sendLocked(voiceKeepAlive, nil)
		case packet == voicePing:
			err = s.sendLocked(voicePing, body)
		}
		s.mu.Unlock()
		if err != nil {
			s.fail(err)
			return
		}
	}
}

func (s *voiceSession) parseInbound(frame []byte) []byte {
	if len(frame) < 2 || frame[0] != voiceMagic {
		return nil
	}
	n, used, ok := voiceReadLength(frame[1:], voiceMaxPayload)
	if !ok || n < 12+16+1 || 1+used+n != len(frame) {
		return nil
	}
	payload := frame[1+used:]
	// Open in place: no receive-side allocation, and invalid tags never reach handlers.
	plain, err := s.crypto.Open(payload[12:12], payload[:12], payload[12:], nil)
	if err != nil {
		return nil
	}
	return plain
}
