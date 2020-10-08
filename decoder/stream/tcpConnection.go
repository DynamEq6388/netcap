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

// This code is based on the gopacket/examples/reassemblydump/main.go example.
// The following license is provided:
// Copyright (c) 2012 Google, Inc. All rights reserved.
// Copyright (c) 2009-2011 Andreas Krennmair. All rights reserved.

// Redistribution and use in source and binary forms, with or without
// modification, are permitted provided that the following conditions are
// met:

//    * Redistributions of source code must retain the above copyright
// notice, this list of conditions and the following disclaimer.
//    * Redistributions in binary form must reproduce the above
// copyright notice, this list of conditions and the following disclaimer
// in the documentation and/or other materials provided with the
// distribution.
//    * Neither the name of Andreas Krennmair, Google, nor the names of its
// contributors may be used to endorse or promote products derived from
// this software without specific prior written permission.

// THIS SOFTWARE IS PROVIDED BY THE COPYRIGHT HOLDERS AND CONTRIBUTORS
// "AS IS" AND ANY EXPRESS OR IMPLIED WARRANTIES, INCLUDING, BUT NOT
// LIMITED TO, THE IMPLIED WARRANTIES OF MERCHANTABILITY AND FITNESS FOR
// A PARTICULAR PURPOSE ARE DISCLAIMED. IN NO EVENT SHALL THE COPYRIGHT
// OWNER OR CONTRIBUTORS BE LIABLE FOR ANY DIRECT, INDIRECT, INCIDENTAL,
// SPECIAL, EXEMPLARY, OR CONSEQUENTIAL DAMAGES (INCLUDING, BUT NOT
// LIMITED TO, PROCUREMENT OF SUBSTITUTE GOODS OR SERVICES; LOSS OF USE,
// DATA, OR PROFITS; OR BUSINESS INTERRUPTION) HOWEVER CAUSED AND ON ANY
// THEORY OF LIABILITY, WHETHER IN CONTRACT, STRICT LIABILITY, OR TORT
// (INCLUDING NEGLIGENCE OR OTHERWISE) ARISING IN ANY WAY OUT OF THE USE
// OF THIS SOFTWARE, EVEN IF ADVISED OF THE POSSIBILITY OF SUCH DAMAGE.

package stream

import (
	"encoding/hex"
	"fmt"
	"log"
	"os"
	"reflect"
	"runtime/pprof"
	"sort"
	"strconv"
	"sync"
	"time"

	"github.com/davecgh/go-spew/spew"
	"github.com/dreadl0ck/gopacket"
	"github.com/dreadl0ck/gopacket/layers"
	"github.com/evilsocket/islazy/tui"
	"go.uber.org/zap"

	"github.com/dreadl0ck/netcap/defaults"
	"github.com/dreadl0ck/netcap/dpi"
	"github.com/dreadl0ck/netcap/reassembly"
	"github.com/dreadl0ck/netcap/utils"
)

var (
	start          = time.Now()
	errorsMap      = make(map[string]uint)
	errorsMapMutex sync.Mutex
)

var stats struct {
	ipdefrag            int64
	missedBytes         int64
	pkt                 int64
	sz                  int64
	totalsz             int64
	rejectFsm           int64
	rejectOpt           int64
	rejectConnFsm       int64
	reassembled         int64
	outOfOrderBytes     int64
	outOfOrderPackets   int64
	biggestChunkBytes   int64
	biggestChunkPackets int64
	overlapBytes        int64
	overlapPackets      int64
	savedTCPConnections int64
	savedUDPConnections int64
	numSoftware         int64
	numServices         int64

	requests  int64
	responses int64
	count     int64
	numErrors uint
	dataBytes int64
	numConns  int64
	numFlows  int64

	// HTTP
	numUnmatchedResp        int64
	numNilRequests          int64
	numFoundRequests        int64
	numRemovedRequests      int64
	numUnansweredRequests   int64
	numClientStreamNotFound int64
	numRequests             int64
	numResponses            int64

	sync.Mutex
}

// NumSavedTCPConns returns the number of saved TCP connections.
func NumSavedTCPConns() int64 {
	stats.Lock()
	defer stats.Unlock()

	return stats.savedTCPConnections
}

// NumSavedUDPConns returns the number of saved UDP conversations.
func NumSavedUDPConns() int64 {
	stats.Lock()
	defer stats.Unlock()

	return stats.savedUDPConnections
}

/*
 * TCP Connection
 */

// internal structure that describes a bi-directional TCP connection
// It implements the reassembly.Stream interface to handle the incoming data
// and manage the stream lifecycle
// this structure has an optimized field order to avoid excessive padding.
type tcpConnection struct {
	net, transport gopacket.Flow

	optchecker reassembly.TCPOptionCheck

	merged      dataFragments
	firstPacket time.Time

	client streamReader
	server streamReader

	ident    string
	decoder  streamDecoder
	tcpstate *reassembly.TCPSimpleFSM

	sync.Mutex

	wasMerged bool
	fsmerr    bool
}

// Accept decides whether the TCP packet should be accepted
// start could be modified to force a start even if no SYN have been seen.
func (t *tcpConnection) Accept(tcp *layers.TCP, dir reassembly.TCPFlowDirection, nextSeq reassembly.Sequence) bool {
	// Finite State Machine
	if !t.tcpstate.CheckState(tcp, dir) {
		logReassemblyError("FSM", fmt.Sprintf("%s: Packet rejected by FSM (state:%s)", t.ident, t.tcpstate.String()), nil)
		stats.Lock()
		stats.rejectFsm++

		if !t.fsmerr {
			t.fsmerr = true
			stats.rejectConnFsm++
		}
		stats.Unlock()

		if !conf.IgnoreFSMerr {
			return false
		}
	}

	// TCP Options
	err := t.optchecker.Accept(tcp, dir, nextSeq)
	if err != nil {
		logReassemblyError("OptionChecker", fmt.Sprintf("%s: packet rejected by OptionChecker", t.ident), err)
		stats.Lock()
		stats.rejectOpt++
		stats.Unlock()

		if !conf.NoOptCheck {
			return false
		}
	}

	// TCP Checksum
	accept := true

	if conf.Checksum {
		chk, errChk := tcp.ComputeChecksum()
		if errChk != nil {
			logReassemblyError("ChecksumCompute", fmt.Sprintf("%s: error computing checksum", t.ident), errChk)

			accept = false
		} else if chk != 0x0 {
			logReassemblyError("Checksum", fmt.Sprintf("%s: invalid checksum: 0x%x", t.ident, chk), nil)

			accept = false
		}
	}

	// stats
	if !accept {
		stats.Lock()
		stats.rejectOpt++
		stats.Unlock()
	}

	return accept
}

func (t *tcpConnection) updateStats(sg reassembly.ScatterGather, skip int, length int, saved int, start bool, end bool, dir reassembly.TCPFlowDirection) {
	sgStats := sg.Stats()

	stats.Lock()
	if skip > 0 {
		stats.missedBytes += int64(skip)
	}

	stats.sz += int64(length - saved)
	stats.pkt += int64(sgStats.Packets)
	if sgStats.Chunks > 1 {
		stats.reassembled++
	}
	stats.outOfOrderPackets += int64(sgStats.QueuedPackets)
	stats.outOfOrderBytes += int64(sgStats.QueuedBytes)

	if int64(length) > stats.biggestChunkBytes {
		stats.biggestChunkBytes = int64(length)
	}

	if int64(sgStats.Packets) > stats.biggestChunkPackets {
		stats.biggestChunkPackets = int64(sgStats.Packets)
	}

	if sgStats.OverlapBytes != 0 && sgStats.OverlapPackets == 0 {
		streamLog.Warn("reassembledSG: invalid overlap",
			zap.Int("bytes", sgStats.OverlapBytes),
			zap.Int("packets", sgStats.OverlapPackets),
		)
	}

	stats.overlapBytes += int64(sgStats.OverlapBytes)
	stats.overlapPackets += int64(sgStats.OverlapPackets)
	stats.Unlock()

	var ident string
	if dir == reassembly.TCPDirClientToServer {
		ident = fmt.Sprintf("%v %v(%s): ", t.net, t.transport, dir)
	} else {
		ident = fmt.Sprintf("%v %v(%s): ", t.net.Reverse(), t.transport.Reverse(), dir)
	}

	reassemblyLog.Debug("SG reassembled packet",
		zap.String("ident", ident),
		zap.Int("length", length),
		zap.Bool("start", start),
		zap.Bool("end", end),
		zap.Int("skip", skip),
		zap.Int("saved", saved),
		zap.Int("packets", sgStats.Packets),
		zap.Int("chunks", sgStats.Chunks),
		zap.Int("overlapBytes", sgStats.OverlapBytes),
		zap.Int("overlapPackets", sgStats.OverlapPackets),
	)
}

func (t *tcpConnection) feedData(dir reassembly.TCPFlowDirection, data []byte, ac reassembly.AssemblerContext) {
	// fmt.Println(t.ident, "feedData", ansi.White, dir, ansi.Cyan, len(data), ansi.Yellow, ac.GetCaptureInfo().Timestamp.Format("2006-02-01 15:04:05.000000"), ansi.Reset)
	// fmt.Println(hex.Dump(data))

	// Copy the data before passing it to the handler
	// Because the passed in buffer can be reused as soon as the ReassembledSG function returned
	dataCpy := make([]byte, len(data))
	l := copy(dataCpy, data)

	if l != len(data) {
		log.Fatal("l != len(data): ", l, " != ", len(data), " ident:", t.ident)
	}

	ti := time.Now()

	// pass data either to client or server
	if dir == reassembly.TCPDirClientToServer {
		t.client.DataChan() <- &streamData{
			rawData: dataCpy,
			ac:      ac,
			dir:     dir,
		}
	} else {
		t.server.DataChan() <- &streamData{
			rawData: dataCpy,
			ac:      ac,
			dir:     dir,
		}
	}

	tcpStreamFeedDataTime.WithLabelValues(dir.String()).Set(float64(time.Since(ti).Nanoseconds()))
}

// ReassembledSG is called zero or more times and delivers the data for a stream
// The ScatterGather buffer is reused after each Reassembled call
// so it's important to copy anything you need out of it (or use KeepFrom()).
func (t *tcpConnection) ReassembledSG(sg reassembly.ScatterGather, ac reassembly.AssemblerContext) {
	length, saved := sg.Lengths()
	dir, startTime, end, skip := sg.Info()

	// update stats
	t.updateStats(sg, skip, length, saved, startTime, end, dir)

	if skip == -1 && conf.AllowMissingInit {
		// this is allowed
	} else if skip != 0 {
		// Missing bytes in stream: do not even try to parse it
		return
	}

	data := sg.Fetch(length)

	// fmt.Println("got raw data:", len(data), ac.GetCaptureInfo().Timestamp, "\n", hex.Dump(data))

	if length > 0 {
		if conf.HexDump {
			reassemblyLog.Debug("feeding stream reader",
				zap.String("data", hex.Dump(data)),
			)
		}

		t.feedData(dir, data, ac)
	}
}

func (t *tcpConnection) reorder(ac reassembly.AssemblerContext, firstFlow gopacket.Flow) {
	// fmt.Println(t.ident, "t.firstPacket:", t.firstPacket, "ac.Timestamp", ac.GetCaptureInfo().Timestamp, "firstFlow", firstFlow)
	// fmt.Println(t.ident, !t.firstPacket.Equal(ac.GetCaptureInfo().Timestamp), "&&", t.firstPacket.After(ac.GetCaptureInfo().Timestamp))

	// is this packet older than the oldest packet we saw for this connection?
	// if yes, if check the direction of the client is correct
	if !t.firstPacket.Equal(ac.GetCaptureInfo().Timestamp) && t.firstPacket.After(ac.GetCaptureInfo().Timestamp) { // update first packet timestamp on connection
		t.Lock()
		t.firstPacket = ac.GetCaptureInfo().Timestamp
		t.Unlock()

		if t.client != nil && t.server != nil {
			// check if firstFlow is identical or needs to be flipped
			if !(t.client.Network() == firstFlow) { // flip
				t.client.SetClient(false)
				t.server.SetClient(true)

				t.Lock()
				t.ident = utils.ReverseFlowIdent(t.ident)
				// fmt.Println("flip! new", ansi.Red+t.ident+ansi.Reset, t.firstPacket)

				t.client, t.server = t.server, t.client
				t.transport, t.net = t.transport.Reverse(), t.net.Reverse()

				// fix directions for all data fragments
				for _, d := range t.client.DataSlice() {
					d.setDirection(reassembly.TCPDirClientToServer)
				}

				for _, d := range t.server.DataSlice() {
					d.setDirection(reassembly.TCPDirServerToClient)
				}
				t.Unlock()
			}
		}
	}
}

// ReassemblyComplete is called when assembly decides there is
// no more data for this stream, either because a FIN or RST packet
// was seen, or because the stream has timed out without any new
// packet data (due to a call to FlushCloseOlderThan).
// It should return true if the connection should be removed from the pool
// It can return false if it want to see subsequent packets with Accept(), e.g. to
// see FIN-ACK, for deeper state-machine analysis.
func (t *tcpConnection) ReassemblyComplete(ac reassembly.AssemblerContext, firstFlow gopacket.Flow, reason string) bool {
	// reorder the stream fragments
	t.reorder(ac, firstFlow)

	streamLog.Debug("ReassemblyComplete",
		zap.String("ident", t.ident),
		zap.String("reason", reason),
		zap.Bool("clientIsNil", t.client == nil),
		zap.Bool("clientSaved:", t.client.Saved()),
		zap.Bool("serverIsNil", t.server == nil),
		zap.Bool("serverSaved:", t.server.Saved()),
	)

	ti := time.Now()

	// save data for the current stream
	if t.server != nil && !t.client.Saved() {
		t.client.MarkSaved()

		t.sortAndMergeFragments()

		// save the full conversation to disk if enabled
		err := saveConversation(protoTCP, t.merged, t.client.Ident(), t.client.FirstPacket(), t.client.Transport())
		if err != nil {
			reassemblyLog.Error("failed to save stream", zap.Error(err), zap.String("ident", t.client.Ident()))
		}
		tcpStreamProcessingTime.WithLabelValues(reassembly.TCPDirClientToServer.String()).Set(float64(time.Since(ti).Nanoseconds()))

		// decode the actual conversation.
		// this needs to be invoked only once, and since ReassemblyComplete is invoked for each side of the connection
		// decode should be called either when processing the client or the server stream
		t.decode()
	}

	if t.server != nil && !t.server.Saved() {
		t.server.MarkSaved()

		t.sortAndMergeFragments()

		// server
		saveTCPServiceBanner(t.server)
		tcpStreamProcessingTime.WithLabelValues(reassembly.TCPDirServerToClient.String()).Set(float64(time.Since(ti).Nanoseconds()))
	}

	reassemblyLog.Debug("stream closed",
		zap.String("ident", t.ident),
	)

	// optionally, do not remove the connection to allow last ACK
	return conf.RemoveClosedStreams
}

func (t *tcpConnection) decode() {
	// choose the decoder to run against the data stream
	var (
		cr, sr = t.client.DataSlice().first(), t.server.DataSlice().first()
		found  bool
	)

	conv := &conversationInfo{
		data:              t.merged,
		ident:             t.ident,
		firstClientPacket: t.client.FirstPacket(),
		firstServerPacket: t.server.FirstPacket(),
		clientIP:          t.client.Network().Src().String(),
		serverIP:          t.client.Network().Dst().String(),
		clientPort:        utils.DecodePort(t.client.Transport().Src().Raw()),
		serverPort:        utils.DecodePort(t.client.Transport().Dst().Raw()),
	}

	// make a good first guess based on the destination port of the connection
	if sd, exists := defaultStreamDecoders[utils.DecodePort(t.server.Transport().Dst().Raw())]; exists {
		if sd.GetReaderFactory() != nil && sd.CanDecode(cr, sr) {
			t.decoder = sd.GetReaderFactory().New(conv)
			found = true
		}
	}

	// if no stream decoder for the port was found, or the stream decoder did not match
	// try all available decoders and use the first one that matches
	if !found {
		for _, sd := range defaultStreamDecoders {
			if sd.GetReaderFactory() != nil && sd.CanDecode(cr, sr) {
				t.decoder = sd.GetReaderFactory().New(conv)
				break
			}
		}
	}

	// call the decoder if one was found
	if t.decoder != nil {
		ti := time.Now()

		// call the associated decoder
		t.decoder.Decode()

		tcpStreamDecodeTime.WithLabelValues(reflect.TypeOf(t.decoder).String()).Set(float64(time.Since(ti).Nanoseconds()))
	}
}

// ReassemblePacket takes care of submitting a TCP packet to the reassembly.
func ReassemblePacket(packet gopacket.Packet, assembler *reassembly.Assembler) {
	// prevent passing any non TCP packets in here
	tcpLayer := packet.Layer(layers.LayerTypeTCP)
	if tcpLayer == nil {
		udpLayer := packet.Layer(layers.LayerTypeUDP)
		if udpLayer != nil {
			udpStreams.handleUDP(packet, udpLayer)
		}

		return
	}

	// lock to sync with read on destroy
	stats.Lock()
	stats.count++
	stats.dataBytes += int64(len(packet.Data()))
	stats.Unlock()

	// defrag the IPv4 packet if desired
	// TODO: implement defragmentation for IPv6
	ip4Layer := packet.Layer(layers.LayerTypeIPv4)
	if ip4Layer != nil && conf.DefragIPv4 {

		var (
			ip4         = ip4Layer.(*layers.IPv4)
			l           = ip4.Length
			newip4, err = streamFactory.defragger.DefragIPv4(ip4)
		)

		if err != nil {
			log.Fatalln("error while de-fragmenting", err)
		} else if newip4 == nil {
			reassemblyLog.Debug("fragment received...")

			return
		}

		if newip4.Length != l {
			stats.ipdefrag++

			reassemblyLog.Debug("decoding re-assembled packet", zap.String("layer", newip4.NextLayerType().String()))

			pb, ok := packet.(gopacket.PacketBuilder)
			if !ok {
				panic("Not a PacketBuilder")
			}

			nextDecoder := newip4.NextLayerType()
			if err = nextDecoder.Decode(newip4.Payload, pb); err != nil {
				fmt.Println("failed to decode ipv4:", err)
			}
		}
	}

	tcp := tcpLayer.(*layers.TCP)

	if conf.Checksum {
		err := tcp.SetNetworkLayerForChecksum(packet.NetworkLayer())
		if err != nil {
			log.Fatalf("Failed to set network layer for checksum: %s\n", err)
		}
	}

	stats.Lock()
	stats.totalsz += int64(len(tcp.Payload))
	stats.Unlock()

	// for debugging:
	// assembleWithContextTimeout(packet, assembler, tcp)
	assembler.AssembleWithContext(packet.NetworkLayer().NetworkFlow(), tcp, &context{
		CaptureInfo: packet.Metadata().CaptureInfo,
	})

	// TODO: refactor and use a ticker model in a goroutine, similar to progress reporting
	if conf.FlushEvery > 0 {
		stats.Lock()
		doFlush := stats.count%int64(conf.FlushEvery) == 0
		stats.Unlock()

		// flush connections in interval
		if doFlush {
			ref := packet.Metadata().CaptureInfo.Timestamp
			flushed, closed := assembler.FlushWithOptions(reassembly.FlushOptions{T: ref.Add(-conf.ClosePendingTimeOut), TC: ref.Add(-conf.CloseInactiveTimeOut)})
			streamLog.Info("forced flush",
				zap.Int("flushed", flushed),
				zap.Int("closed", closed),
				zap.Time("ref", ref),
			)
		}
	}
}

// assembleWithContextTimeout is a function that times out with a log message after a specified interval
// when the stream reassembly gets stuck
// used for debugging.
//goland:noinspection GoUnusedFunction
func assembleWithContextTimeout(packet gopacket.Packet, assembler *reassembly.Assembler, tcp *layers.TCP) {
	done := make(chan bool, 1)

	go func() {
		assembler.AssembleWithContext(packet.NetworkLayer().NetworkFlow(), tcp, &context{
			CaptureInfo: packet.Metadata().CaptureInfo,
		})
		done <- true
	}()

	select {
	case <-done:
	case <-time.After(5 * time.Second):
		spew.Dump(packet.Metadata().CaptureInfo)
		fmt.Println("HTTP AssembleWithContext timeout", packet.NetworkLayer().NetworkFlow(), packet.TransportLayer().TransportFlow())
		fmt.Println(assembler.Dump())
	}
}

// CleanupReassembly will shutdown the reassembly.
func CleanupReassembly(wait bool, assemblers []*reassembly.Assembler) {
	conf.Lock()
	if conf.Debug {
		streamLog.Info("streamPool:")
		streamLog.Info(streamFactory.StreamPool.DumpString())
	}
	conf.Unlock()

	// wait for stream reassembly to finish
	if conf.WaitForConnections || wait {

		streamLog.Info("waiting for last streams to finish processing...")

		// wait for remaining connections to finish processing
		// will wait forever if there are streams that are never shutdown via FIN/RST
		select {
		case <-waitForConns():
		case <-time.After(defaults.ReassemblyTimeout):
			if !conf.Quiet {
				streamLog.Info(" timeout after", zap.Duration("reassembly_timeout", defaults.ReassemblyTimeout))
			}
		}

		if !conf.Quiet {
			fmt.Println("\nprocessing last TCP streams")
		}

		// flush assemblers
		// must be done after waiting for connections or there might be data loss
		for i, a := range assemblers {
			streamLog.Info("flushing tcp assembler",
				zap.Int("current", i+1),
				zap.Int("numAssemblers", len(assemblers)),
			)

			if i == 0 && (!conf.Quiet || conf.PrintProgress) {
				// only display progress bar for the first flush, since all following ones will be instant.
				streamLog.Info("assembler flush", zap.Int("closed", a.FlushAllProgress()))
			} else {
				streamLog.Info("assembler flush", zap.Int("closed", a.FlushAll()))
			}
		}

		streamFactory.Lock()
		numTotal := len(streamFactory.streamReaders)
		streamFactory.Unlock()

		flushTCPStreams(numTotal)
		flushUDPStreams(udpStreams.size())
	}

	if dpi.IsEnabled() {
		// teardown DPI C libs
		dpi.Destroy()
	}

	// create a memory snapshot for debugging
	if conf.MemProfile != "" {
		f, err := os.Create(conf.MemProfile)
		if err != nil {
			log.Fatal(err)
		}

		if err = pprof.WriteHeapProfile(f); err != nil {
			log.Fatal("failed to write heap profile:", err)
		}

		if err = f.Close(); err != nil {
			log.Fatal("failed to close heap profile file:", err)
		}
	}

	// print stats if not quiet
	if !conf.Quiet {
		errorsMapMutex.Lock()
		stats.Lock()
		streamLog.Info("HTTPDecoder stats",
			zap.Int64("packets", stats.count),
			zap.Int64("bytes", stats.dataBytes),
			zap.Duration("duration", time.Since(start)),
			zap.Uint("numErrors", stats.numErrors),
			zap.Int("len(errorsMap)", len(errorsMap)),
			zap.Int64("requests", stats.requests),
			zap.Int64("responses", stats.responses),
		)
		stats.Unlock()
		errorsMapMutex.Unlock()

		// print configuration
		// print configuration as table
		tui.Table(reassemblyLogFileHandle, []string{"Reassembly Setting", "Value"}, [][]string{
			{"FlushEvery", strconv.Itoa(conf.FlushEvery)},
			{"CloseInactiveTimeout", conf.CloseInactiveTimeOut.String()},
			{"ClosePendingTimeout", conf.ClosePendingTimeOut.String()},
			{"AllowMissingInit", strconv.FormatBool(conf.AllowMissingInit)},
			{"IgnoreFsmErr", strconv.FormatBool(conf.IgnoreFSMerr)},
			{"NoOptCheck", strconv.FormatBool(conf.NoOptCheck)},
			{"Checksum", strconv.FormatBool(conf.Checksum)},
			{"DefragIPv4", strconv.FormatBool(conf.DefragIPv4)},
			{"WriteIncomplete", strconv.FormatBool(conf.WriteIncomplete)},
		})

		printProgress(1, 1)

		stats.Lock()

		var rows [][]string
		if conf.DefragIPv4 {
			rows = append(rows, []string{"IPv4 defragmentation", strconv.FormatInt(stats.ipdefrag, 10)})
		}

		rows = append(rows,
			[]string{"missed bytes", strconv.FormatInt(stats.missedBytes, 10)},
			[]string{"total packets", strconv.FormatInt(stats.pkt, 10)},
			[]string{"rejected FSM", strconv.FormatInt(stats.rejectFsm, 10)},
			[]string{"rejected Options", strconv.FormatInt(stats.rejectOpt, 10)},
			[]string{"reassembled bytes", strconv.FormatInt(stats.sz, 10)},
			[]string{"total TCP bytes", strconv.FormatInt(stats.totalsz, 10)},
			[]string{"connection rejected FSM", strconv.FormatInt(stats.rejectConnFsm, 10)},
			[]string{"reassembled chunks", strconv.FormatInt(stats.reassembled, 10)},
			[]string{"out-of-order packets", strconv.FormatInt(stats.outOfOrderPackets, 10)},
			[]string{"out-of-order bytes", strconv.FormatInt(stats.outOfOrderBytes, 10)},
			[]string{"biggest-chunk packets", strconv.FormatInt(stats.biggestChunkPackets, 10)},
			[]string{"biggest-chunk bytes", strconv.FormatInt(stats.biggestChunkBytes, 10)},
			[]string{"overlap packets", strconv.FormatInt(stats.overlapPackets, 10)},
			[]string{"overlap bytes", strconv.FormatInt(stats.overlapBytes, 10)},
			[]string{"saved TCP connections", strconv.FormatInt(stats.savedTCPConnections, 10)},
			[]string{"saved UDP conversations", strconv.FormatInt(stats.savedUDPConnections, 10)},
			[]string{"numSoftware", strconv.FormatInt(stats.numSoftware, 10)},
			[]string{"numServices", strconv.FormatInt(stats.numServices, 10)},
		)
		stats.Unlock()

		tui.Table(reassemblyLogFileHandle, []string{"TCP Stat", "Value"}, rows)

		errorsMapMutex.Lock()
		stats.Lock()
		if stats.numErrors != 0 {
			rows = [][]string{}
			for e := range errorsMap {
				rows = append(rows, []string{e, strconv.FormatUint(uint64(errorsMap[e]), 10)})
			}

			tui.Table(reassemblyLogFileHandle, []string{"Error Subject", "Count"}, rows)
		}

		stats.Unlock()
		errorsMapMutex.Unlock()
	}
}

func waitForConns() chan struct{} {
	out := make(chan struct{})

	go func() {
		// WaitGoRoutines waits until the goroutines launched to process TCP streams are done
		// this will block forever if there are streams that are never shutdown (via RST or FIN flags)
		streamFactory.waitGoRoutines()
		out <- struct{}{}
	}()

	return out
}

// sort the conversation fragments and fill the conversation buffers.
func (t *tcpConnection) sortAndMergeFragments() {
	t.Lock()
	if !t.wasMerged {

		// only do this once per connection
		t.wasMerged = true

		// concatenate both client and server data fragments
		t.merged = append(t.client.DataSlice(), t.server.DataSlice()...)

		// sort based on their timestamps
		sort.Sort(t.merged)
	}
	t.Unlock()
}
