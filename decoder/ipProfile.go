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

package decoder

import (
	decoderutils "github.com/dreadl0ck/netcap/decoder/utils"
	"log"
	"sync"
	"sync/atomic"

	"github.com/dreadl0ck/gopacket"
	"github.com/dreadl0ck/ja3"
	"github.com/dreadl0ck/tlsx"
	"github.com/gogo/protobuf/proto"

	"github.com/dreadl0ck/netcap/dpi"
	"github.com/dreadl0ck/netcap/resolvers"
	"github.com/dreadl0ck/netcap/types"
	"github.com/dreadl0ck/netcap/utils"
)

var (
	// LocalDNS controls whether the DNS names shall be resolved locally
	// without contacting a nameserver.
	LocalDNS = true

	ipProfileDecoderInstance *PacketDecoder
)

// atomicIPProfileMap contains all connections and provides synchronized access.
type atomicIPProfileMap struct {
	// SrcIP to DeviceProfiles
	Items map[string]*ipProfile
	sync.Mutex
}

// Size returns the number of elements in the Items map.
func (a *atomicIPProfileMap) Size() int {
	a.Lock()
	defer a.Unlock()

	return len(a.Items)
}

// IPProfiles contains a map of IP specific behavior profiles at runtime.
var IPProfiles = &atomicIPProfileMap{
	Items: make(map[string]*ipProfile),
}

// wrapper for the types.IPProfile that can be locked.
type ipProfile struct {
	*types.IPProfile
	sync.Mutex
}

var ipProfileDecoder = NewPacketDecoder(
	types.Type_NC_IPProfile,
	"IPProfile",
	"An IPProfile contains information about a single IPv4 or IPv6 address seen on the network and it's behavior",
	func(d *PacketDecoder) error {
		ipProfileDecoderInstance = d

		return nil
	},
	func(p gopacket.Packet) proto.Message {
		return nil
	},
	func(e *PacketDecoder) error {
		// flush writer
		for _, item := range IPProfiles.Items {
			item.Lock()
			writeIPProfile(item.IPProfile)
			item.Unlock()
		}

		return nil
	},
)

// GetIPProfile fetches a known profile and updates it or returns a new one.
func getIPProfile(ipAddr string, i *decoderutils.PacketInfo, source bool) *ipProfile {
	if ipAddr == "" {
		return nil
	}

	IPProfiles.Lock()
	if p, ok := IPProfiles.Items[ipAddr]; ok {
		IPProfiles.Unlock()

		p.Lock()

		p.NumPackets++
		p.TimestampLast = i.Timestamp

		dataLen := uint64(len(i.Packet.Data()))
		p.Bytes += dataLen

		// Transport Layer
		if tl := i.Packet.TransportLayer(); tl != nil {
			if source {
				doSrcPortUpdate(p, utils.DecodePort(tl.TransportFlow().Src().Raw()), tl.LayerType().String(), dataLen)
				doContactedPortUpdate(p, utils.DecodePort(tl.TransportFlow().Dst().Raw()), tl.LayerType().String(), dataLen)
			} else {
				doDstPortUpdate(p, utils.DecodePort(tl.TransportFlow().Dst().Raw()), tl.LayerType().String(), dataLen)
				doContactedPortUpdate(p, utils.DecodePort(tl.TransportFlow().Src().Raw()), tl.LayerType().String(), dataLen)
			}
		}

		// Session Layer: TLS
		ch := tlsx.GetClientHelloBasic(i.Packet)
		if ch != nil {
			if ch.SNI != "" {
				p.SNIs[ch.SNI]++
			}
		}

		ja3Hash := ja3.DigestHexPacket(i.Packet)
		if ja3Hash == "" {
			ja3Hash = ja3.DigestHexPacketJa3s(i.Packet)
		}

		if ja3Hash != "" {
			// add hash to profile if not already present
			if _, ok = p.Ja3[ja3Hash]; !ok {
				p.Ja3[ja3Hash] = resolvers.LookupJa3(ja3Hash)
			}
		}

		// Application Layer: DPI
		uniqueResults := dpi.GetProtocols(i.Packet)
		for protocol, res := range uniqueResults {
			// check if proto exists already
			var prot *types.Protocol
			if prot, ok = p.Protocols[protocol]; ok {
				prot.Packets++
			} else {
				// add new
				p.Protocols[protocol] = dpi.NewProto(&res)
			}
		}

		p.Unlock()

		return p
	}
	IPProfiles.Unlock()

	var (
		protos  = make(map[string]*types.Protocol)
		ja3Map  = make(map[string]string)
		dataLen = uint64(len(i.Packet.Data()))
		sniMap  = make(map[string]int64)
	)

	// Network Layer: IP Geolocation
	loc, _ := resolvers.LookupGeolocation(ipAddr)

	// Transport Layer: Port information
	srcPorts, dstPorts, contactedPorts := initPorts(i, source)

	// Session Layer: TLS

	ja3Hash := ja3.DigestHexPacket(i.Packet)
	if ja3Hash == "" {
		ja3Hash = ja3.DigestHexPacketJa3s(i.Packet)
	}

	if ja3Hash != "" {
		ja3Map[ja3Hash] = resolvers.LookupJa3(ja3Hash)
	}

	ch := tlsx.GetClientHelloBasic(i.Packet)
	if ch != nil {
		sniMap[ch.SNI] = 1
	}

	// Application Layer: DPI
	uniqueResults := dpi.GetProtocols(i.Packet)
	for protocol, res := range uniqueResults {
		protos[protocol] = dpi.NewProto(&res)
	}

	var names []string
	if LocalDNS {
		if name := resolvers.LookupDNSNameLocal(ipAddr); len(name) != 0 {
			names = append(names, name)
		}
	} else {
		names = resolvers.LookupDNSNames(ipAddr)
	}

	// create new profile
	p := &ipProfile{
		IPProfile: &types.IPProfile{
			Addr:           ipAddr,
			NumPackets:     1,
			Geolocation:    loc,
			DNSNames:       names,
			TimestampFirst: i.Timestamp,
			Ja3:            ja3Map,
			Protocols:      protos,
			Bytes:          dataLen,
			SrcPorts:       srcPorts,
			DstPorts:       dstPorts,
			ContactedPorts: contactedPorts,
			SNIs:           sniMap,
		},
	}

	IPProfiles.Lock()
	IPProfiles.Items[ipAddr] = p
	IPProfiles.Unlock()

	return p
}

func doSrcPortUpdate(p *ipProfile, srcPort int32, layerType string, dataLen uint64) {
	var found bool

	// source port
	for _, port := range p.SrcPorts {
		if port.PortNumber == srcPort && port.Protocol == layerType {

			atomic.AddUint64(&port.Bytes, dataLen)
			atomic.AddUint64(&port.Packets, 1)

			found = true

			break
		}
	}

	if !found {
		p.SrcPorts = append(p.SrcPorts, &types.Port{
			PortNumber: srcPort,
			Bytes:      dataLen,
			Packets:    1,
			Protocol:   layerType,
		})
	}
}

func doContactedPortUpdate(p *ipProfile, dstPort int32, layerType string, dataLen uint64) {
	var found bool

	for _, port := range p.ContactedPorts {
		if port.PortNumber == dstPort && port.Protocol == layerType {

			atomic.AddUint64(&port.Bytes, dataLen)
			atomic.AddUint64(&port.Packets, 1)

			found = true

			break
		}
	}

	if !found {
		p.ContactedPorts = append(p.ContactedPorts, &types.Port{
			PortNumber: dstPort,
			Bytes:      dataLen,
			Packets:    1,
			Protocol:   layerType,
		})
	}
}

func doDstPortUpdate(p *ipProfile, dstPort int32, layerType string, dataLen uint64) {
	var found bool

	// destination port
	for _, port := range p.DstPorts {
		if port.PortNumber == dstPort && port.Protocol == layerType {
			atomic.AddUint64(&port.Bytes, dataLen)
			atomic.AddUint64(&port.Packets, 1)

			found = true

			break
		}
	}

	if !found {
		p.DstPorts = append(p.DstPorts, &types.Port{
			PortNumber: dstPort,
			Bytes:      dataLen,
			Packets:    1,
			Protocol:   layerType,
		})
	}
}

func initPorts(i *decoderutils.PacketInfo, source bool) (
	srcPorts,
	dstPorts,
	contactedPorts []*types.Port,
) {
	if tl := i.Packet.TransportLayer(); tl != nil {
		// get packet size
		dataLen := uint64(len(i.Packet.Data()))

		if source {
			// source port
			srcPorts = append(srcPorts, &types.Port{
				PortNumber: utils.DecodePort(tl.TransportFlow().Src().Raw()),
				Bytes:      dataLen,
				Packets:    1,
				Protocol:   tl.LayerType().String(),
			})
			// contacted port
			contactedPorts = append(contactedPorts, &types.Port{
				PortNumber: utils.DecodePort(tl.TransportFlow().Dst().Raw()),
				Bytes:      dataLen,
				Packets:    1,
				Protocol:   tl.LayerType().String(),
			})
		} else {
			// destination port
			dstPorts = append(dstPorts, &types.Port{
				PortNumber: utils.DecodePort(tl.TransportFlow().Dst().Raw()),
				Bytes:      dataLen,
				Packets:    1,
				Protocol:   tl.LayerType().String(),
			})
			// contacted port
			contactedPorts = append(contactedPorts, &types.Port{
				PortNumber: utils.DecodePort(tl.TransportFlow().Src().Raw()),
				Bytes:      dataLen,
				Packets:    1,
				Protocol:   tl.LayerType().String(),
			})
		}
	}

	return
}

// writeIPProfile writes the ip profile.
func writeIPProfile(i *types.IPProfile) {
	if conf.ExportMetrics {
		i.Inc()
	}

	atomic.AddInt64(&ipProfileDecoderInstance.NumRecordsWritten, 1)

	err := ipProfileDecoderInstance.Writer.Write(i)
	if err != nil {
		log.Fatal("failed to write proto: ", err)
	}
}
