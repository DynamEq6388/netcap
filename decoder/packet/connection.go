/*
 * NETCAP - Traffic Analysis Framework
 * Copyright (c) 2017-2020 Philipp Mieden <dreadl0ck [at] protonmail [dot] ch>
 *
 * THE SOFTWARE IS PROVIDED "AS IS" AND THE AUTHOR DISCLAIMS ALL WARRANTIES
 * WITH REGARD TO THIS SOFTWARE INCLUDING ALL IMPLIED WARRANTIES OF
 * MERCHANTABILITY AND FITNESS. IN NO EVENT SHALL THE AUTHOR BE LIABLE FOR
 * ANY SPECIAL, DIRECT, INDIRECT, OR CONSEQUENTIAL DAMAGES OR ANY DAMAGES
 * WHATSOEVER RESULTING FROM LOSS OF USE, DATA OR PROFITS, WHETHER IN AN
 * ACTION OF CONTRACT, NEGLIGENCE OR OTHER TORTIOUS ACTION, ARISING OUT OF
 * OR IN CONNECTION WITH THE USE OR PERFORMANCE OF THIS SOFTWARE.
 */

package packet

import (
	"log"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	"github.com/dreadl0ck/gopacket"
	"github.com/gogo/protobuf/proto"

	"github.com/dreadl0ck/netcap/types"
)

// connectionID is a bidirectional connection
// between two devices over the network
// that includes the Link, Network and TransportLayer.
type connectionID struct {
	LinkFlowID      uint64
	NetworkFlowID   uint64
	TransportFlowID uint64
}

func (c connectionID) String() string {
	return strconv.FormatUint(c.LinkFlowID, 10) + strconv.FormatUint(c.NetworkFlowID, 10) + strconv.FormatUint(c.TransportFlowID, 10)
}

type connection struct {
	*types.Connection
	sync.Mutex
}

// atomicConnMap contains all connections and provides synchronized access.
type atomicConnMap struct {
	Items map[string]*connection
	sync.Mutex
}

// Size returns the number of elements in the Items map.
func (a *atomicConnMap) Size() int {
	a.Lock()
	defer a.Unlock()

	return len(a.Items)
}

type connectionDecoder struct {
	*PacketDecoder
	Conns *atomicConnMap
}

var connDecoder = &connectionDecoder{
	PacketDecoder: &PacketDecoder{
		Type:        types.Type_NC_Connection,
		Name:        "Connection",
		Description: "A connection represents bi-directional network communication between two hosts based on the combined link-, network- and transport layer identifiers",
	},
	Conns: &atomicConnMap{
		Items: make(map[string]*connection),
	},
}

// PostInit is called after the decoder has been initialized.
func (cd *connectionDecoder) PostInit() error {
	// simply overwrite the handler with our custom one
	// this way the CustomEncoders default Decode() implementation will be used
	// (it takes care of applying config options and tracking stats)
	// but with our custom logic to handle the actual packet
	cd.Handler = cd.handlePacket

	return nil
}

// Destroy closes and flushes all writers and calls deinit if set.
func (cd *connectionDecoder) Destroy() (name string, size int64) {
	err := cd.DeInit()
	if err != nil {
		panic(err)
	}

	return cd.Writer.Close(cd.NumRecordsWritten)
}

func (cd *connectionDecoder) handlePacket(p gopacket.Packet) proto.Message {
	// assemble connectionID
	connID := connectionID{}
	if ll := p.LinkLayer(); ll != nil {
		connID.LinkFlowID = ll.LinkFlow().FastHash()
	}

	if nl := p.NetworkLayer(); nl != nil {
		connID.NetworkFlowID = nl.NetworkFlow().FastHash()
	}

	if tl := p.TransportLayer(); tl != nil {
		connID.TransportFlowID = tl.TransportFlow().FastHash()
	}

	// lookup flow
	cd.Conns.Lock()

	if conn, ok := cd.Conns.Items[connID.String()]; ok {

		// connID exists. update fields
		calcDuration := false

		conn.Lock()

		// check if received packet from the same connection
		// was captured BEFORE the connections first seen timestamp
		if p.Metadata().Timestamp.Before(time.Unix(0, conn.TimestampFirst).UTC()) {
			calcDuration = true

			// rewrite timestamp
			conn.TimestampFirst = p.Metadata().Timestamp.UnixNano()

			// rewrite source and destination parameters
			// since the first packet decides about the connection direction
			if ll := p.LinkLayer(); ll != nil {
				conn.SrcMAC = ll.LinkFlow().Src().String()
				conn.DstMAC = ll.LinkFlow().Dst().String()
			}

			if nl := p.NetworkLayer(); nl != nil {
				conn.SrcIP = nl.NetworkFlow().Src().String()
				conn.DstIP = nl.NetworkFlow().Dst().String()
			}

			if tl := p.TransportLayer(); tl != nil {
				conn.SrcPort = tl.TransportFlow().Src().String()
				conn.DstPort = tl.TransportFlow().Dst().String()
			}
		}

		// check if last timestamp was before the current packet
		if conn.TimestampLast < p.Metadata().Timestamp.UnixNano() {
			// current packet is newer
			// update last seen timestamp
			conn.TimestampLast = p.Metadata().Timestamp.UnixNano()
			calcDuration = true
		} // else: do nothing, timestamp is still the oldest one

		conn.NumPackets++
		conn.TotalSize += int32(len(p.Data()))

		// only calculate duration when timestamps have changed
		// TODO: calculate duration once when stream is flushed for performance
		if calcDuration {
			conn.Duration = time.Unix(0, conn.TimestampLast).Sub(time.Unix(0, conn.TimestampFirst)).Nanoseconds()
		}

		conn.Unlock()
	} else { // create a new Connection
		co := &types.Connection{}
		co.UID = calcMd5(connID.String())
		co.TimestampFirst = p.Metadata().Timestamp.UnixNano()
		co.TimestampLast = p.Metadata().Timestamp.UnixNano()

		if ll := p.LinkLayer(); ll != nil {
			co.LinkProto = ll.LayerType().String()
			co.SrcMAC = ll.LinkFlow().Src().String()
			co.DstMAC = ll.LinkFlow().Dst().String()
		}
		if nl := p.NetworkLayer(); nl != nil {
			co.NetworkProto = nl.LayerType().String()
			co.SrcIP = nl.NetworkFlow().Src().String()
			co.DstIP = nl.NetworkFlow().Dst().String()
		}
		if tl := p.TransportLayer(); tl != nil {
			co.TransportProto = tl.LayerType().String()
			co.SrcPort = tl.TransportFlow().Src().String()
			co.DstPort = tl.TransportFlow().Dst().String()
		}
		if al := p.ApplicationLayer(); al != nil {
			co.ApplicationProto = al.LayerType().String()
			co.AppPayloadSize = int32(len(al.Payload()))
		}
		cd.Conns.Items[connID.String()] = &connection{
			Connection: co,
		}

		// TODO: add dedicated stats structure for decoder pkg
		// conns := atomic.AddInt64(&stream.stats.numConns, 1)

		// flush
		//if conf.ConnFlushInterval != 0 && conns%int64(conf.ConnFlushInterval) == 0 {
		//	cd.flushConns(p)
		//}
	}
	cd.Conns.Unlock()

	return nil
}

func (cd *connectionDecoder) flushConns(p gopacket.Packet) {
	var selectConns []*types.Connection

	for id, entry := range cd.Conns.Items {
		// flush entries whose last timestamp is connTimeOut older than current packet
		if p.Metadata().Timestamp.Sub(time.Unix(0, entry.TimestampLast)) > conf.ConnTimeOut {
			selectConns = append(selectConns, entry.Connection)
			// cleanup
			delete(cd.Conns.Items, id)
		}
	}

	// flush selection in background
	go func() {
		for _, selectedConn := range selectConns {
			cd.writeConn(selectedConn)
		}
	}()
}

// DeInit is called prior to teardown.
func (cd *connectionDecoder) DeInit() error {
	cd.Conns.Lock()
	for _, f := range cd.Conns.Items {
		f.Lock()
		cd.writeConn(f.Connection)
		f.Unlock()
	}
	cd.Conns.Unlock()

	return nil
}

// writeConn writes the connection.
func (cd *connectionDecoder) writeConn(conn *types.Connection) {
	if conf.ExportMetrics {
		conn.Inc()
	}

	atomic.AddInt64(&cd.NumRecordsWritten, 1)

	err := cd.Writer.Write(conn)
	if err != nil {
		log.Fatal("failed to write proto: ", err)
	}
}
