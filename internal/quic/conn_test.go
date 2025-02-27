// Copyright 2023 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

//go:build go1.21

package quic

import (
	"errors"
	"fmt"
	"math"
	"net/netip"
	"reflect"
	"strings"
	"testing"
	"time"
)

func TestConnTestConn(t *testing.T) {
	tc := newTestConn(t, serverSide)
	if got, want := tc.timeUntilEvent(), defaultMaxIdleTimeout; got != want {
		t.Errorf("new conn timeout=%v, want %v (max_idle_timeout)", got, want)
	}

	var ranAt time.Time
	tc.conn.runOnLoop(func(now time.Time, c *Conn) {
		ranAt = now
	})
	if !ranAt.Equal(tc.now) {
		t.Errorf("func ran on loop at %v, want %v", ranAt, tc.now)
	}
	tc.wait()

	nextTime := tc.now.Add(defaultMaxIdleTimeout / 2)
	tc.advanceTo(nextTime)
	tc.conn.runOnLoop(func(now time.Time, c *Conn) {
		ranAt = now
	})
	if !ranAt.Equal(nextTime) {
		t.Errorf("func ran on loop at %v, want %v", ranAt, nextTime)
	}
	tc.wait()

	tc.advanceToTimer()
	if !tc.conn.exited {
		t.Errorf("after advancing to idle timeout, exited = false, want true")
	}
}

type testDatagram struct {
	packets    []*testPacket
	paddedSize int
}

func (d testDatagram) String() string {
	var b strings.Builder
	fmt.Fprintf(&b, "datagram with %v packets", len(d.packets))
	if d.paddedSize > 0 {
		fmt.Fprintf(&b, " (padded to %v bytes)", d.paddedSize)
	}
	b.WriteString(":")
	for _, p := range d.packets {
		b.WriteString("\n")
		b.WriteString(p.String())
	}
	return b.String()
}

type testPacket struct {
	ptype     packetType
	version   uint32
	num       packetNumber
	dstConnID []byte
	srcConnID []byte
	frames    []debugFrame
}

func (p testPacket) String() string {
	var b strings.Builder
	fmt.Fprintf(&b, "  %v %v", p.ptype, p.num)
	if p.version != 0 {
		fmt.Fprintf(&b, " version=%v", p.version)
	}
	if p.srcConnID != nil {
		fmt.Fprintf(&b, " src={%x}", p.srcConnID)
	}
	if p.dstConnID != nil {
		fmt.Fprintf(&b, " dst={%x}", p.dstConnID)
	}
	for _, f := range p.frames {
		fmt.Fprintf(&b, "\n    %v", f)
	}
	return b.String()
}

// A testConn is a Conn whose external interactions (sending and receiving packets,
// setting timers) can be manipulated in tests.
type testConn struct {
	t              *testing.T
	conn           *Conn
	now            time.Time
	timer          time.Time
	timerLastFired time.Time
	idlec          chan struct{} // only accessed on the conn's loop

	// Read and write keys are distinct from the conn's keys,
	// because the test may know about keys before the conn does.
	// For example, when sending a datagram with coalesced
	// Initial and Handshake packets to a client conn,
	// we use Handshake keys to encrypt the packet.
	// The client only acquires those keys when it processes
	// the Initial packet.
	rkeys [numberSpaceCount]keys // for packets sent to the conn
	wkeys [numberSpaceCount]keys // for packets sent by the conn

	// Information about the conn's (fake) peer.
	peerConnID        []byte                         // source conn id of peer's packets
	peerNextPacketNum [numberSpaceCount]packetNumber // next packet number to use

	// Datagrams, packets, and frames sent by the conn,
	// but not yet processed by the test.
	sentDatagrams       [][]byte
	sentPackets         []*testPacket
	sentFrames          []debugFrame
	sentFramePacketType packetType

	// Frame types to ignore in tests.
	ignoreFrames map[byte]bool
}

// newTestConn creates a Conn for testing.
//
// The Conn's event loop is controlled by the test,
// allowing test code to access Conn state directly
// by first ensuring the loop goroutine is idle.
func newTestConn(t *testing.T, side connSide) *testConn {
	t.Helper()
	tc := &testConn{
		t:          t,
		now:        time.Date(2020, 1, 1, 0, 0, 0, 0, time.UTC),
		peerConnID: []byte{0xa0, 0xa1, 0xa2, 0xa3, 0xa4, 0xa5},
		ignoreFrames: map[byte]bool{
			frameTypePadding: true, // ignore PADDING by default
		},
	}
	t.Cleanup(tc.cleanup)

	var initialConnID []byte
	if side == serverSide {
		// The initial connection ID for the server is chosen by the client.
		// When creating a server-side connection, pick a random connection ID here.
		var err error
		initialConnID, err = newRandomConnID()
		if err != nil {
			tc.t.Fatal(err)
		}
	}

	conn, err := newConn(
		tc.now,
		side,
		initialConnID,
		netip.MustParseAddrPort("127.0.0.1:443"),
		(*testConnListener)(tc),
		(*testConnHooks)(tc))
	if err != nil {
		tc.t.Fatal(err)
	}
	tc.conn = conn

	tc.wkeys[initialSpace] = conn.tlsState.wkeys[initialSpace]
	tc.rkeys[initialSpace] = conn.tlsState.rkeys[initialSpace]

	tc.wait()
	return tc
}

// advance causes time to pass.
func (tc *testConn) advance(d time.Duration) {
	tc.t.Helper()
	tc.advanceTo(tc.now.Add(d))
}

// advanceTo sets the current time.
func (tc *testConn) advanceTo(now time.Time) {
	tc.t.Helper()
	if tc.now.After(now) {
		tc.t.Fatalf("time moved backwards: %v -> %v", tc.now, now)
	}
	tc.now = now
	if tc.timer.After(tc.now) {
		return
	}
	tc.conn.sendMsg(timerEvent{})
	tc.wait()
}

// advanceToTimer sets the current time to the time of the Conn's next timer event.
func (tc *testConn) advanceToTimer() {
	if tc.timer.IsZero() {
		tc.t.Fatalf("advancing to timer, but timer is not set")
	}
	tc.advanceTo(tc.timer)
}

func (tc *testConn) timerDelay() time.Duration {
	if tc.timer.IsZero() {
		return math.MaxInt64 // infinite
	}
	if tc.timer.Before(tc.now) {
		return 0
	}
	return tc.timer.Sub(tc.now)
}

const infiniteDuration = time.Duration(math.MaxInt64)

// timeUntilEvent returns the amount of time until the next connection event.
func (tc *testConn) timeUntilEvent() time.Duration {
	if tc.timer.IsZero() {
		return infiniteDuration
	}
	if tc.timer.Before(tc.now) {
		return 0
	}
	return tc.timer.Sub(tc.now)
}

// wait blocks until the conn becomes idle.
// The conn is idle when it is blocked waiting for a packet to arrive or a timer to expire.
// Tests shouldn't need to call wait directly.
// testConn methods that wake the Conn event loop will call wait for them.
func (tc *testConn) wait() {
	tc.t.Helper()
	idlec := make(chan struct{})
	fail := false
	tc.conn.sendMsg(func(now time.Time, c *Conn) {
		if tc.idlec != nil {
			tc.t.Errorf("testConn.wait called concurrently")
			fail = true
			close(idlec)
		} else {
			// nextMessage will close idlec.
			tc.idlec = idlec
		}
	})
	select {
	case <-idlec:
	case <-tc.conn.donec:
	}
	if fail {
		panic(fail)
	}
}

func (tc *testConn) cleanup() {
	if tc.conn == nil {
		return
	}
	tc.conn.exit()
}

// write sends the Conn a datagram.
func (tc *testConn) write(d *testDatagram) {
	tc.t.Helper()
	var buf []byte
	for _, p := range d.packets {
		space := spaceForPacketType(p.ptype)
		if p.num >= tc.peerNextPacketNum[space] {
			tc.peerNextPacketNum[space] = p.num + 1
		}
		buf = append(buf, tc.encodeTestPacket(p)...)
	}
	for len(buf) < d.paddedSize {
		buf = append(buf, 0)
	}
	tc.conn.sendMsg(&datagram{
		b: buf,
	})
	tc.wait()
}

// writeFrame sends the Conn a datagram containing the given frames.
func (tc *testConn) writeFrames(ptype packetType, frames ...debugFrame) {
	tc.t.Helper()
	space := spaceForPacketType(ptype)
	dstConnID := tc.conn.connIDState.local[0].cid
	if tc.conn.connIDState.local[0].seq == -1 && ptype != packetTypeInitial {
		// Only use the transient connection ID in Initial packets.
		dstConnID = tc.conn.connIDState.local[1].cid
	}
	d := &testDatagram{
		packets: []*testPacket{{
			ptype:     ptype,
			num:       tc.peerNextPacketNum[space],
			frames:    frames,
			version:   1,
			dstConnID: dstConnID,
			srcConnID: tc.peerConnID,
		}},
	}
	if ptype == packetTypeInitial && tc.conn.side == serverSide {
		d.paddedSize = 1200
	}
	tc.write(d)
}

// ignoreFrame hides frames of the given type sent by the Conn.
func (tc *testConn) ignoreFrame(frameType byte) {
	tc.ignoreFrames[frameType] = true
}

// readDatagram reads the next datagram sent by the Conn.
// It returns nil if the Conn has no more datagrams to send at this time.
func (tc *testConn) readDatagram() *testDatagram {
	tc.t.Helper()
	tc.wait()
	tc.sentPackets = nil
	tc.sentFrames = nil
	if len(tc.sentDatagrams) == 0 {
		return nil
	}
	buf := tc.sentDatagrams[0]
	tc.sentDatagrams = tc.sentDatagrams[1:]
	return tc.parseTestDatagram(buf)
}

// readPacket reads the next packet sent by the Conn.
// It returns nil if the Conn has no more packets to send at this time.
func (tc *testConn) readPacket() *testPacket {
	tc.t.Helper()
	for len(tc.sentPackets) == 0 {
		d := tc.readDatagram()
		if d == nil {
			return nil
		}
		tc.sentPackets = d.packets
	}
	p := tc.sentPackets[0]
	tc.sentPackets = tc.sentPackets[1:]
	return p
}

// readFrame reads the next frame sent by the Conn.
// It returns nil if the Conn has no more frames to send at this time.
func (tc *testConn) readFrame() (debugFrame, packetType) {
	tc.t.Helper()
	for len(tc.sentFrames) == 0 {
		p := tc.readPacket()
		if p == nil {
			return nil, packetTypeInvalid
		}
		tc.sentFramePacketType = p.ptype
		tc.sentFrames = p.frames
	}
	f := tc.sentFrames[0]
	tc.sentFrames = tc.sentFrames[1:]
	return f, tc.sentFramePacketType
}

// wantDatagram indicates that we expect the Conn to send a datagram.
func (tc *testConn) wantDatagram(expectation string, want *testDatagram) {
	tc.t.Helper()
	got := tc.readDatagram()
	if !reflect.DeepEqual(got, want) {
		tc.t.Fatalf("%v:\ngot datagram:  %v\nwant datagram: %v", expectation, got, want)
	}
}

// wantPacket indicates that we expect the Conn to send a packet.
func (tc *testConn) wantPacket(expectation string, want *testPacket) {
	tc.t.Helper()
	got := tc.readPacket()
	if !reflect.DeepEqual(got, want) {
		tc.t.Fatalf("%v:\ngot packet:  %v\nwant packet: %v", expectation, got, want)
	}
}

// wantFrame indicates that we expect the Conn to send a frame.
func (tc *testConn) wantFrame(expectation string, wantType packetType, want debugFrame) {
	tc.t.Helper()
	got, gotType := tc.readFrame()
	if got == nil {
		tc.t.Fatalf("%v:\nconnection is idle\nwant %v frame: %v", expectation, wantType, want)
	}
	if gotType != wantType {
		tc.t.Fatalf("%v:\ngot %v packet, want %v", expectation, wantType, want)
	}
	if !reflect.DeepEqual(got, want) {
		tc.t.Fatalf("%v:\ngot frame:  %v\nwant frame: %v", expectation, got, want)
	}
}

// wantIdle indicates that we expect the Conn to not send any more frames.
func (tc *testConn) wantIdle(expectation string) {
	tc.t.Helper()
	switch {
	case len(tc.sentFrames) > 0:
		tc.t.Fatalf("expect: %v\nunexpectedly got: %v", expectation, tc.sentFrames[0])
	case len(tc.sentPackets) > 0:
		tc.t.Fatalf("expect: %v\nunexpectedly got: %v", expectation, tc.sentPackets[0])
	}
	if f, _ := tc.readFrame(); f != nil {
		tc.t.Fatalf("expect: %v\nunexpectedly got: %v", expectation, f)
	}
}

func (tc *testConn) encodeTestPacket(p *testPacket) []byte {
	tc.t.Helper()
	var w packetWriter
	w.reset(1200)
	var pnumMaxAcked packetNumber
	if p.ptype != packetType1RTT {
		w.startProtectedLongHeaderPacket(pnumMaxAcked, longPacket{
			ptype:     p.ptype,
			version:   p.version,
			num:       p.num,
			dstConnID: p.dstConnID,
			srcConnID: p.srcConnID,
		})
	} else {
		w.start1RTTPacket(p.num, pnumMaxAcked, p.dstConnID)
	}
	for _, f := range p.frames {
		f.write(&w)
	}
	space := spaceForPacketType(p.ptype)
	if !tc.rkeys[space].isSet() {
		tc.t.Fatalf("sending packet with no %v keys available", space)
		return nil
	}
	if p.ptype != packetType1RTT {
		w.finishProtectedLongHeaderPacket(pnumMaxAcked, tc.rkeys[space], longPacket{
			ptype:     p.ptype,
			version:   p.version,
			num:       p.num,
			dstConnID: p.dstConnID,
			srcConnID: p.srcConnID,
		})
	} else {
		w.finish1RTTPacket(p.num, pnumMaxAcked, p.dstConnID, tc.rkeys[space])
	}
	return w.datagram()
}

func (tc *testConn) parseTestDatagram(buf []byte) *testDatagram {
	tc.t.Helper()
	bufSize := len(buf)
	d := &testDatagram{}
	for len(buf) > 0 {
		if buf[0] == 0 {
			d.paddedSize = bufSize
			break
		}
		ptype := getPacketType(buf)
		space := spaceForPacketType(ptype)
		if !tc.wkeys[space].isSet() {
			tc.t.Fatalf("no keys for space %v, packet type %v", space, ptype)
		}
		if isLongHeader(buf[0]) {
			var pnumMax packetNumber // TODO: Track packet numbers.
			p, n := parseLongHeaderPacket(buf, tc.wkeys[space], pnumMax)
			if n < 0 {
				tc.t.Fatalf("packet parse error")
			}
			frames, err := tc.parseTestFrames(p.payload)
			if err != nil {
				tc.t.Fatal(err)
			}
			d.packets = append(d.packets, &testPacket{
				ptype:     p.ptype,
				version:   p.version,
				num:       p.num,
				dstConnID: p.dstConnID,
				srcConnID: p.srcConnID,
				frames:    frames,
			})
			buf = buf[n:]
		} else {
			var pnumMax packetNumber // TODO: Track packet numbers.
			p, n := parse1RTTPacket(buf, tc.wkeys[space], len(tc.peerConnID), pnumMax)
			if n < 0 {
				tc.t.Fatalf("packet parse error")
			}
			dstConnID, _ := dstConnIDForDatagram(buf)
			frames, err := tc.parseTestFrames(p.payload)
			if err != nil {
				tc.t.Fatal(err)
			}
			d.packets = append(d.packets, &testPacket{
				ptype:     packetType1RTT,
				num:       p.num,
				dstConnID: dstConnID,
				frames:    frames,
			})
			buf = buf[n:]
		}
	}
	return d
}

func (tc *testConn) parseTestFrames(payload []byte) ([]debugFrame, error) {
	tc.t.Helper()
	var frames []debugFrame
	for len(payload) > 0 {
		f, n := parseDebugFrame(payload)
		if n < 0 {
			return nil, errors.New("error parsing frames")
		}
		if !tc.ignoreFrames[payload[0]] {
			frames = append(frames, f)
		}
		payload = payload[n:]
	}
	return frames, nil
}

func spaceForPacketType(ptype packetType) numberSpace {
	switch ptype {
	case packetTypeInitial:
		return initialSpace
	case packetType0RTT:
		panic("TODO: packetType0RTT")
	case packetTypeHandshake:
		return handshakeSpace
	case packetTypeRetry:
		panic("TODO: packetTypeRetry")
	case packetType1RTT:
		return appDataSpace
	}
	panic("unknown packet type")
}

// testConnHooks implements connTestHooks.
type testConnHooks testConn

// nextMessage is called by the Conn's event loop to request its next event.
func (tc *testConnHooks) nextMessage(msgc chan any, timer time.Time) (now time.Time, m any) {
	tc.timer = timer
	if !timer.IsZero() && !timer.After(tc.now) {
		if timer.Equal(tc.timerLastFired) {
			// If the connection timer fires at time T, the Conn should take some
			// action to advance the timer into the future. If the Conn reschedules
			// the timer for the same time, it isn't making progress and we have a bug.
			tc.t.Errorf("connection timer spinning; now=%v timer=%v", tc.now, timer)
		} else {
			tc.timerLastFired = timer
			return tc.now, timerEvent{}
		}
	}
	select {
	case m := <-msgc:
		return tc.now, m
	default:
	}
	// If the message queue is empty, then the conn is idle.
	if tc.idlec != nil {
		idlec := tc.idlec
		tc.idlec = nil
		close(idlec)
	}
	m = <-msgc
	return tc.now, m
}

// testConnListener implements connListener.
type testConnListener testConn

func (tc *testConnListener) sendDatagram(p []byte, addr netip.AddrPort) error {
	tc.sentDatagrams = append(tc.sentDatagrams, append([]byte(nil), p...))
	return nil
}
