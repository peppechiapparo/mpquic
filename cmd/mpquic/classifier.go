package main

import (
	"encoding/binary"
	"fmt"
	"net/netip"
	"strconv"
	"strings"
)

type compiledDataplane struct {
	defaultClass string
	classes      map[string]DataplaneClassPolicy
	classifiers  []compiledClassifierRule
}

type compiledClassifierRule struct {
	name      string
	className string
	protocol  string
	srcCIDRs  []netip.Prefix
	dstCIDRs  []netip.Prefix
	srcPorts  []portRange
	dstPorts  []portRange
	dscp      map[uint8]struct{}
}

type portRange struct {
	from uint16
	to   uint16
}

type packetMeta struct {
	protocol string
	srcAddr  netip.Addr
	dstAddr  netip.Addr
	srcPort  uint16
	dstPort  uint16
	hasPorts bool
	dscp     uint8
}

type trafficClassCounters struct {
	txPackets    uint64
	txErrors     uint64
	txDuplicates uint64
}

func compileDataplaneConfig(dp DataplaneConfig) (compiledDataplane, error) {
	out := compiledDataplane{
		defaultClass: dp.DefaultClass,
		classes:      make(map[string]DataplaneClassPolicy, len(dp.Classes)),
	}

	for className, policy := range dp.Classes {
		out.classes[className] = policy
	}

	out.classifiers = make([]compiledClassifierRule, 0, len(dp.Classifiers))
	for _, rule := range dp.Classifiers {
		srcCIDRs, err := parseCIDRs(rule.SrcCIDRs)
		if err != nil {
			return compiledDataplane{}, err
		}
		dstCIDRs, err := parseCIDRs(rule.DstCIDRs)
		if err != nil {
			return compiledDataplane{}, err
		}
		srcPorts, err := parsePortRanges(rule.SrcPorts)
		if err != nil {
			return compiledDataplane{}, err
		}
		dstPorts, err := parsePortRanges(rule.DstPorts)
		if err != nil {
			return compiledDataplane{}, err
		}
		dscp := make(map[uint8]struct{}, len(rule.DSCP))
		for _, value := range rule.DSCP {
			dscp[uint8(value)] = struct{}{}
		}

		out.classifiers = append(out.classifiers, compiledClassifierRule{
			name:      rule.Name,
			className: rule.ClassName,
			protocol:  rule.Protocol,
			srcCIDRs:  srcCIDRs,
			dstCIDRs:  dstCIDRs,
			srcPorts:  srcPorts,
			dstPorts:  dstPorts,
			dscp:      dscp,
		})
	}

	return out, nil
}

func parseCIDRs(values []string) ([]netip.Prefix, error) {
	if len(values) == 0 {
		return nil, nil
	}
	out := make([]netip.Prefix, 0, len(values))
	for _, raw := range values {
		v := strings.TrimSpace(raw)
		if v == "" {
			continue
		}
		prefix, err := netip.ParsePrefix(v)
		if err != nil {
			return nil, fmt.Errorf("%q: %w", v, err)
		}
		out = append(out, prefix)
	}
	return out, nil
}

func parsePortRanges(values []string) ([]portRange, error) {
	if len(values) == 0 {
		return nil, nil
	}
	out := make([]portRange, 0, len(values))
	for _, raw := range values {
		v := strings.TrimSpace(raw)
		if v == "" {
			continue
		}
		if strings.Contains(v, "-") {
			parts := strings.SplitN(v, "-", 2)
			start, err := strconv.Atoi(strings.TrimSpace(parts[0]))
			if err != nil {
				return nil, fmt.Errorf("invalid port range start %q", v)
			}
			end, err := strconv.Atoi(strings.TrimSpace(parts[1]))
			if err != nil {
				return nil, fmt.Errorf("invalid port range end %q", v)
			}
			if start < 1 || start > 65535 || end < 1 || end > 65535 || end < start {
				return nil, fmt.Errorf("invalid port range %q", v)
			}
			out = append(out, portRange{from: uint16(start), to: uint16(end)})
			continue
		}

		port, err := strconv.Atoi(v)
		if err != nil || port < 1 || port > 65535 {
			return nil, fmt.Errorf("invalid port value %q", v)
		}
		out = append(out, portRange{from: uint16(port), to: uint16(port)})
	}
	return out, nil
}

func (r compiledClassifierRule) matches(meta packetMeta) bool {
	if r.protocol != "" && r.protocol != meta.protocol {
		return false
	}
	if len(r.srcCIDRs) > 0 && !matchAddrPrefixes(meta.srcAddr, r.srcCIDRs) {
		return false
	}
	if len(r.dstCIDRs) > 0 && !matchAddrPrefixes(meta.dstAddr, r.dstCIDRs) {
		return false
	}
	if len(r.srcPorts) > 0 {
		if !meta.hasPorts || !matchPortRanges(meta.srcPort, r.srcPorts) {
			return false
		}
	}
	if len(r.dstPorts) > 0 {
		if !meta.hasPorts || !matchPortRanges(meta.dstPort, r.dstPorts) {
			return false
		}
	}
	if len(r.dscp) > 0 {
		if _, ok := r.dscp[meta.dscp]; !ok {
			return false
		}
	}
	return true
}

func matchAddrPrefixes(addr netip.Addr, prefixes []netip.Prefix) bool {
	if !addr.IsValid() {
		return false
	}
	for _, p := range prefixes {
		if p.Contains(addr) {
			return true
		}
	}
	return false
}

func matchPortRanges(port uint16, ranges []portRange) bool {
	for _, r := range ranges {
		if port >= r.from && port <= r.to {
			return true
		}
	}
	return false
}

func parsePacketMeta(pkt []byte) (packetMeta, bool) {
	if len(pkt) < 1 {
		return packetMeta{}, false
	}

	version := pkt[0] >> 4
	switch version {
	case 4:
		if len(pkt) < 20 {
			return packetMeta{}, false
		}
		ihl := int(pkt[0]&0x0f) * 4
		if ihl < 20 || len(pkt) < ihl {
			return packetMeta{}, false
		}

		src := netip.AddrFrom4([4]byte{pkt[12], pkt[13], pkt[14], pkt[15]})
		dst := netip.AddrFrom4([4]byte{pkt[16], pkt[17], pkt[18], pkt[19]})
		meta := packetMeta{
			protocol: ipProtocolName(pkt[9]),
			srcAddr:  src,
			dstAddr:  dst,
			dscp:     pkt[1] >> 2,
		}
		if (meta.protocol == "tcp" || meta.protocol == "udp") && len(pkt) >= ihl+4 {
			meta.srcPort = binary.BigEndian.Uint16(pkt[ihl : ihl+2])
			meta.dstPort = binary.BigEndian.Uint16(pkt[ihl+2 : ihl+4])
			meta.hasPorts = true
		}
		return meta, true

	case 6:
		if len(pkt) < 40 {
			return packetMeta{}, false
		}

		var srcArr, dstArr [16]byte
		copy(srcArr[:], pkt[8:24])
		copy(dstArr[:], pkt[24:40])
		trafficClass := ((pkt[0] & 0x0f) << 4) | (pkt[1] >> 4)
		meta := packetMeta{
			protocol: ipProtocolName(pkt[6]),
			srcAddr:  netip.AddrFrom16(srcArr),
			dstAddr:  netip.AddrFrom16(dstArr),
			dscp:     trafficClass >> 2,
		}
		if (meta.protocol == "tcp" || meta.protocol == "udp") && len(pkt) >= 44 {
			meta.srcPort = binary.BigEndian.Uint16(pkt[40:42])
			meta.dstPort = binary.BigEndian.Uint16(pkt[42:44])
			meta.hasPorts = true
		}
		return meta, true
	}

	return packetMeta{}, false
}

func ipProtocolName(proto uint8) string {
	switch proto {
	case 6:
		return "tcp"
	case 17:
		return "udp"
	case 1:
		return "icmp"
	case 58:
		return "icmpv6"
	default:
		return strconv.Itoa(int(proto))
	}
}
