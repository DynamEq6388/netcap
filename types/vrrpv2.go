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

package types

import (
	"strings"
	"time"

	"github.com/prometheus/client_golang/prometheus"
)

var fieldsVRRPv2 = []string{
	"Timestamp",
	"Version",      // int32
	"Type",         // int32
	"VirtualRtrID", // int32
	"Priority",     // int32
	"CountIPAddr",  // int32
	"AuthType",     // int32
	"AdverInt",     // int32
	"Checksum",     // int32
	"IPAddresses",  // []string
	"SrcIP",
	"DstIP",
}

// CSVHeader returns the CSV header for the audit record.
func (a *VRRPv2) CSVHeader() []string {
	return filter(fieldsVRRPv2)
}

// CSVRecord returns the CSV record for the audit record.
func (a *VRRPv2) CSVRecord() []string {
	return filter([]string{
		formatTimestamp(a.Timestamp),
		formatInt32(a.Version),      // int32
		formatInt32(a.Type),         // int32
		formatInt32(a.VirtualRtrID), // int32
		formatInt32(a.Priority),     // int32
		formatInt32(a.CountIPAddr),  // int32
		formatInt32(a.AuthType),     // int32
		formatInt32(a.AdverInt),     // int32
		formatInt32(a.Checksum),     // int32
		join(a.IPAddress...),        // []string
		a.SrcIP,
		a.DstIP,
	})
}

// Time returns the timestamp associated with the audit record.
func (a *VRRPv2) Time() int64 {
	return a.Timestamp
}

// JSON returns the JSON representation of the audit record.
func (a *VRRPv2) JSON() (string, error) {
	// convert unix timestamp from nano to millisecond precision for elastic
	a.Timestamp /= int64(time.Millisecond)

	return jsonMarshaler.MarshalToString(a)
}

var vrrp2Metric = prometheus.NewCounterVec(
	prometheus.CounterOpts{
		Name: strings.ToLower(Type_NC_VRRPv2.String()),
		Help: Type_NC_VRRPv2.String() + " audit records",
	},
	fieldsVRRPv2[1:],
)

// Inc increments the metrics for the audit record.
func (a *VRRPv2) Inc() {
	vrrp2Metric.WithLabelValues(a.CSVRecord()[1:]...).Inc()
}

// SetPacketContext sets the associated packet context for the audit record.
func (a *VRRPv2) SetPacketContext(ctx *PacketContext) {
	a.SrcIP = ctx.SrcIP
	a.DstIP = ctx.DstIP
}

// Src returns the source address of the audit record.
func (a *VRRPv2) Src() string {
	return a.SrcIP
}

// Dst returns the destination address of the audit record.
func (a *VRRPv2) Dst() string {
	return a.DstIP
}
