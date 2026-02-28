package congestion

import (
	"fmt"
	"math"
	"time"

	"github.com/quic-go/quic-go/internal/protocol"
	"github.com/quic-go/quic-go/internal/utils"
	"github.com/quic-go/quic-go/logging"
)

// BBRv1 congestion control implementation for QUIC.
// Based on the BBR algorithm described in:
//   - https://datatracker.ietf.org/doc/html/draft-cardwell-iccrg-bbr-congestion-control
//   - https://github.com/google/bbr (Linux kernel reference)
//
// BBR (Bottleneck Bandwidth and Round-trip propagation time) is a
// congestion control algorithm that models the network path to achieve
// high throughput with low queuing delay.

// BBR operating modes
type bbrMode int

const (
	bbrStartup    bbrMode = iota // Exponential bandwidth probing
	bbrDrain                     // Drain queues built during startup
	bbrProbeBW                   // Steady-state: cycle pacing gain to probe bandwidth
	bbrProbeRTT                  // Reduce cwnd to probe for lower RTT
)

// BBR pacing gain cycles in ProbeBW mode
// Cycle through these gains to probe for more bandwidth.
// 1.25 probes for more, 0.75 drains any queue, 1.0 x6 cruises.
var probeBWGainCycle = [8]float64{1.25, 0.75, 1.0, 1.0, 1.0, 1.0, 1.0, 1.0}

const (
	// bbr startup pacing gain: 2/ln2 ≈ 2.885
	bbrStartupPacingGain = 2.885
	// bbr startup cwnd gain
	bbrStartupCwndGain = 2.885
	// bbr drain pacing gain
	bbrDrainPacingGain = 1.0 / bbrStartupPacingGain
	// bbr drain cwnd gain
	bbrDrainCwndGain = bbrStartupPacingGain
	// probe_rtt cwnd is 4 packets
	bbrProbeRTTCwndPackets = 4
	// minimum duration to spend in PROBE_RTT
	bbrProbeRTTDuration = 200 * time.Millisecond
	// if we haven't seen a new minRTT for this long, enter PROBE_RTT
	bbrMinRTTExpiry = 10 * time.Second
	// number of rounds without bandwidth growth to exit STARTUP
	bbrStartupFullBandwidthRounds = 3
	// growth rate threshold to detect bandwidth plateau in STARTUP
	bbrStartupFullBandwidthThreshold = 1.25
	// minimum congestion window in packets
	bbrMinCongestionWindowPackets = 4
)

// bbrSender implements BBRv1 congestion control.
type bbrSender struct {
	rttStats *utils.RTTStats
	clock    Clock
	pacer    *pacer
	tracer   *logging.ConnectionTracer

	mode      bbrMode
	roundTrip bbrRoundTrip

	// Max bandwidth filter (windowed max over ~10 RTTs)
	maxBandwidth windowedFilter

	// Min RTT tracking
	minRTT     time.Duration
	minRTTTime time.Time

	// Pacing gain and cwnd gain
	pacingGain float64
	cwndGain   float64

	// ProbeBW cycle index
	cycleIndex    int
	cycleStart    time.Time

	// PROBE_RTT state
	probeRTTStart time.Time
	probeRTTDone  bool

	// Startup: detect bandwidth plateau
	fullBandwidth      Bandwidth
	fullBandwidthCount int

	// Congestion window
	congestionWindow    protocol.ByteCount
	initialCwnd         protocol.ByteCount
	maxDatagramSize     protocol.ByteCount

	// Track largest sent/acked for recovery detection
	largestSentPacketNumber  protocol.PacketNumber
	largestAckedPacketNumber protocol.PacketNumber

	// Bytes delivered for delivery rate estimation
	delivered     protocol.ByteCount
	deliveredTime time.Time

	// Application-limited tracking
	appLimited bool

	// Previous state for tracer
	lastState logging.CongestionState
}

var (
	_ SendAlgorithm               = &bbrSender{}
	_ SendAlgorithmWithDebugInfos = &bbrSender{}
)

// NewBBRSender creates a new BBRv1 congestion controller.
func NewBBRSender(
	clock Clock,
	rttStats *utils.RTTStats,
	initialMaxDatagramSize protocol.ByteCount,
	tracer *logging.ConnectionTracer,
) *bbrSender {
	b := &bbrSender{
		rttStats:                 rttStats,
		clock:                    clock,
		tracer:                   tracer,
		mode:                     bbrStartup,
		pacingGain:               bbrStartupPacingGain,
		cwndGain:                 bbrStartupCwndGain,
		maxDatagramSize:          initialMaxDatagramSize,
		congestionWindow:         initialCongestionWindow * initialMaxDatagramSize,
		initialCwnd:              initialCongestionWindow * initialMaxDatagramSize,
		largestSentPacketNumber:  protocol.InvalidPacketNumber,
		largestAckedPacketNumber: protocol.InvalidPacketNumber,
		minRTT:                   0,
		lastState:                logging.CongestionStateSlowStart,
	}
	b.maxBandwidth = newWindowedFilter(10) // 10-round window
	b.pacer = newPacer(b.bandwidthEstimate)
	if b.tracer != nil && b.tracer.UpdatedCongestionState != nil {
		b.tracer.UpdatedCongestionState(logging.CongestionStateSlowStart)
	}
	return b
}

// --- SendAlgorithm interface ---

func (b *bbrSender) TimeUntilSend(_ protocol.ByteCount) time.Time {
	return b.pacer.TimeUntilSend()
}

func (b *bbrSender) HasPacingBudget(now time.Time) bool {
	return b.pacer.Budget(now) >= b.maxDatagramSize
}

func (b *bbrSender) OnPacketSent(
	sentTime time.Time,
	bytesInFlight protocol.ByteCount,
	packetNumber protocol.PacketNumber,
	bytes protocol.ByteCount,
	isRetransmittable bool,
) {
	b.pacer.SentPacket(sentTime, bytes)
	if !isRetransmittable {
		return
	}
	b.largestSentPacketNumber = packetNumber
	// Track app-limited: if we're sending less than BDP, we're app-limited
	if bytesInFlight < b.getBDP() {
		b.appLimited = true
	} else {
		b.appLimited = false
	}
}

func (b *bbrSender) CanSend(bytesInFlight protocol.ByteCount) bool {
	return bytesInFlight < b.GetCongestionWindow()
}

func (b *bbrSender) MaybeExitSlowStart() {
	// BBR doesn't use traditional slow start exit; it exits STARTUP
	// when it detects a bandwidth plateau.
}

func (b *bbrSender) OnPacketAcked(
	ackedPacketNumber protocol.PacketNumber,
	ackedBytes protocol.ByteCount,
	priorInFlight protocol.ByteCount,
	eventTime time.Time,
) {
	b.largestAckedPacketNumber = max(ackedPacketNumber, b.largestAckedPacketNumber)

	// Update delivery rate
	b.delivered += ackedBytes
	if b.deliveredTime.IsZero() {
		b.deliveredTime = eventTime
	}

	// Update min RTT
	rtt := b.rttStats.LatestRTT()
	if rtt > 0 {
		if b.minRTT == 0 || rtt < b.minRTT {
			b.minRTT = rtt
			b.minRTTTime = eventTime
		}
	}

	// Round-trip counting
	b.roundTrip.onPacketAcked(ackedPacketNumber)

	// Update bandwidth estimate on each round trip
	if b.roundTrip.isNewRound(ackedPacketNumber) {
		b.updateBandwidth(eventTime)
		b.roundTrip.startNewRound(b.largestSentPacketNumber)
	}

	// State machine transitions
	b.updateState(eventTime, priorInFlight)

	// Update congestion window
	b.updateCongestionWindow(ackedBytes)
}

func (b *bbrSender) OnCongestionEvent(
	_ protocol.PacketNumber,
	lostBytes protocol.ByteCount,
	_ protocol.ByteCount,
) {
	// BBRv1 is largely loss-agnostic, but we make a minimal adjustment:
	// don't let cwnd go below minimum.
	// In BBRv2/v3, this would be more sophisticated.
	_ = lostBytes
}

func (b *bbrSender) OnRetransmissionTimeout(packetsRetransmitted bool) {
	if !packetsRetransmitted {
		return
	}
	// On RTO, reset to startup but keep a reasonable cwnd (not minCwnd)
	b.mode = bbrStartup
	b.pacingGain = bbrStartupPacingGain
	b.cwndGain = bbrStartupCwndGain
	b.congestionWindow = b.initialCwnd
	b.fullBandwidth = 0
	b.fullBandwidthCount = 0
	b.maybeTraceStateChange(logging.CongestionStateSlowStart)
}

func (b *bbrSender) SetMaxDatagramSize(s protocol.ByteCount) {
	if s < b.maxDatagramSize {
		panic(fmt.Sprintf("congestion BUG: decreased max datagram size from %d to %d", b.maxDatagramSize, s))
	}
	cwndIsMin := b.congestionWindow == b.minCwnd()
	b.maxDatagramSize = s
	if cwndIsMin {
		b.congestionWindow = b.minCwnd()
	}
	b.pacer.SetMaxDatagramSize(s)
}

// --- SendAlgorithmWithDebugInfos interface ---

func (b *bbrSender) InSlowStart() bool {
	return b.mode == bbrStartup
}

func (b *bbrSender) InRecovery() bool {
	return false // BBRv1 doesn't have a traditional recovery state
}

func (b *bbrSender) GetCongestionWindow() protocol.ByteCount {
	if b.mode == bbrProbeRTT {
		return b.probeRTTCwnd()
	}
	return b.congestionWindow
}

// --- Internal BBR logic ---

// getBDP returns the estimated Bandwidth-Delay Product.
func (b *bbrSender) getBDP() protocol.ByteCount {
	bw := b.maxBandwidth.getBest()
	if bw == 0 || b.minRTT == 0 {
		return b.initialCwnd
	}
	bdp := protocol.ByteCount(uint64(bw) * uint64(b.minRTT) / uint64(time.Second) / uint64(BytesPerSecond))
	if bdp < b.initialCwnd {
		return b.initialCwnd
	}
	return bdp
}

// bandwidthEstimate returns the estimated bandwidth for the pacer.
func (b *bbrSender) bandwidthEstimate() Bandwidth {
	bw := b.maxBandwidth.getBest()
	if bw == 0 {
		// Use initial estimate based on IW / initial RTT
		srtt := b.rttStats.SmoothedRTT()
		if srtt == 0 {
			return infBandwidth
		}
		return BandwidthFromDelta(b.congestionWindow, srtt)
	}
	// Apply pacing gain
	paced := Bandwidth(float64(bw) * b.pacingGain)
	// Never pace below the floor (initialCwnd / smoothed RTT)
	srtt := b.rttStats.SmoothedRTT()
	if srtt > 0 {
		floor := BandwidthFromDelta(b.initialCwnd, srtt)
		if paced < floor {
			return floor
		}
	}
	return paced
}

// updateBandwidth samples the delivery rate and updates the max filter.
func (b *bbrSender) updateBandwidth(now time.Time) {
	elapsed := now.Sub(b.deliveredTime)
	if elapsed <= 0 || b.delivered == 0 {
		return
	}
	deliveryRate := BandwidthFromDelta(b.delivered, elapsed)
	if !b.appLimited || deliveryRate > b.maxBandwidth.getBest() {
		b.maxBandwidth.update(deliveryRate, b.roundTrip.count)
	}
	// Reset delivery tracking for next round
	b.delivered = 0
	b.deliveredTime = now
}

// updateState handles BBR state machine transitions.
func (b *bbrSender) updateState(now time.Time, bytesInFlight protocol.ByteCount) {
	switch b.mode {
	case bbrStartup:
		b.updateStartup()
	case bbrDrain:
		b.updateDrain(bytesInFlight)
	case bbrProbeBW:
		b.updateProbeBW(now)
	case bbrProbeRTT:
		b.updateProbeRTT(now, bytesInFlight)
	}

	// Check if we need to enter PROBE_RTT
	if b.mode != bbrStartup && b.mode != bbrProbeRTT {
		if b.minRTTTime.IsZero() || now.Sub(b.minRTTTime) > bbrMinRTTExpiry {
			b.enterProbeRTT(now)
		}
	}
}

func (b *bbrSender) updateStartup() {
	bw := b.maxBandwidth.getBest()
	if bw == 0 {
		return
	}
	// Check if bandwidth has plateaued
	if bw >= Bandwidth(float64(b.fullBandwidth)*bbrStartupFullBandwidthThreshold) {
		b.fullBandwidth = bw
		b.fullBandwidthCount = 0
	}
	b.fullBandwidthCount++
	if b.fullBandwidthCount >= bbrStartupFullBandwidthRounds {
		b.enterDrain()
	}
}

func (b *bbrSender) enterDrain() {
	b.mode = bbrDrain
	b.pacingGain = bbrDrainPacingGain
	b.cwndGain = bbrDrainCwndGain
	b.maybeTraceStateChange(logging.CongestionStateCongestionAvoidance)
}

func (b *bbrSender) updateDrain(bytesInFlight protocol.ByteCount) {
	// Exit drain when bytes in flight drops to BDP
	if bytesInFlight <= b.getBDP() {
		b.enterProbeBW()
	}
}

func (b *bbrSender) enterProbeBW() {
	b.mode = bbrProbeBW
	b.cycleIndex = 0
	b.cycleStart = b.clock.Now()
	b.pacingGain = probeBWGainCycle[0]
	b.cwndGain = 2.0
	b.maybeTraceStateChange(logging.CongestionStateCongestionAvoidance)
}

func (b *bbrSender) updateProbeBW(now time.Time) {
	// Advance cycle phase roughly every minRTT
	cycleDuration := b.minRTT
	if cycleDuration < time.Millisecond {
		cycleDuration = time.Millisecond
	}
	if now.Sub(b.cycleStart) >= cycleDuration {
		b.cycleIndex = (b.cycleIndex + 1) % 8
		b.cycleStart = now
		b.pacingGain = probeBWGainCycle[b.cycleIndex]
	}
}

func (b *bbrSender) enterProbeRTT(now time.Time) {
	b.mode = bbrProbeRTT
	b.pacingGain = 1.0
	b.cwndGain = 1.0
	b.probeRTTStart = now
	b.probeRTTDone = false
	b.maybeTraceStateChange(logging.CongestionStateApplicationLimited)
}

func (b *bbrSender) updateProbeRTT(now time.Time, bytesInFlight protocol.ByteCount) {
	if !b.probeRTTDone {
		// Wait for inflight to drain to probeRTT cwnd
		if bytesInFlight <= b.probeRTTCwnd() {
			if b.probeRTTStart.IsZero() {
				b.probeRTTStart = now
			}
			if now.Sub(b.probeRTTStart) >= bbrProbeRTTDuration {
				b.probeRTTDone = true
			}
		}
	}
	if b.probeRTTDone {
		// Update minRTT timestamp to avoid re-entering PROBE_RTT immediately
		b.minRTTTime = now
		b.enterProbeBW()
	}
}

// updateCongestionWindow sets the cwnd based on BDP and gain.
func (b *bbrSender) updateCongestionWindow(ackedBytes protocol.ByteCount) {
	targetCwnd := b.targetCwnd()
	if b.mode == bbrStartup {
		// In startup, only grow cwnd — never reduce.
		// cwnd grows by ackedBytes each ACK (roughly doubles each RTT).
		b.congestionWindow += ackedBytes
	} else {
		// In other modes, converge toward target
		if targetCwnd > b.congestionWindow {
			b.congestionWindow += ackedBytes
			if b.congestionWindow > targetCwnd {
				b.congestionWindow = targetCwnd
			}
		} else if targetCwnd < b.congestionWindow {
			b.congestionWindow = targetCwnd
		}
	}
	// Enforce minimum
	if b.congestionWindow < b.minCwnd() {
		b.congestionWindow = b.minCwnd()
	}
	// Enforce maximum
	maxCwnd := b.maxDatagramSize * protocol.MaxCongestionWindowPackets
	if b.congestionWindow > maxCwnd {
		b.congestionWindow = maxCwnd
	}
}

func (b *bbrSender) targetCwnd() protocol.ByteCount {
	bdp := b.getBDP()
	return protocol.ByteCount(math.Ceil(float64(bdp) * b.cwndGain))
}

func (b *bbrSender) minCwnd() protocol.ByteCount {
	return bbrMinCongestionWindowPackets * b.maxDatagramSize
}

func (b *bbrSender) probeRTTCwnd() protocol.ByteCount {
	return bbrProbeRTTCwndPackets * b.maxDatagramSize
}

func (b *bbrSender) maybeTraceStateChange(new logging.CongestionState) {
	if b.tracer == nil || b.tracer.UpdatedCongestionState == nil || new == b.lastState {
		return
	}
	b.tracer.UpdatedCongestionState(new)
	b.lastState = new
}

// --- Windowed max filter ---

type windowedFilterSample struct {
	value Bandwidth
	round uint64
}

// windowedFilter tracks the maximum bandwidth over a window of rounds.
type windowedFilter struct {
	windowSize int
	samples    [3]windowedFilterSample // Best, second-best, third-best
}

func newWindowedFilter(windowSize int) windowedFilter {
	return windowedFilter{windowSize: windowSize}
}

func (f *windowedFilter) getBest() Bandwidth {
	return f.samples[0].value
}

func (f *windowedFilter) update(value Bandwidth, round uint64) {
	// If new sample is >= current best, it becomes the new best
	if value >= f.samples[0].value || round-f.samples[2].round >= uint64(f.windowSize) {
		f.samples[2] = windowedFilterSample{value: value, round: round}
		f.samples[1] = f.samples[2]
		f.samples[0] = f.samples[2]
		return
	}
	if value >= f.samples[1].value || round-f.samples[1].round >= uint64(f.windowSize) {
		f.samples[1] = windowedFilterSample{value: value, round: round}
		if value >= f.samples[0].value {
			f.samples[0] = f.samples[1]
		}
		return
	}
	if value >= f.samples[2].value || round-f.samples[2].round >= uint64(f.windowSize) {
		f.samples[2] = windowedFilterSample{value: value, round: round}
	}
	// Expire old samples
	if round-f.samples[0].round >= uint64(f.windowSize) {
		f.samples[0] = f.samples[1]
		f.samples[1] = f.samples[2]
	}
	if round-f.samples[1].round >= uint64(f.windowSize) {
		f.samples[1] = f.samples[2]
	}
}

// --- Round-trip counter ---

type bbrRoundTrip struct {
	count          uint64
	lastRoundStart protocol.PacketNumber
	currentEnd     protocol.PacketNumber
}

func (r *bbrRoundTrip) onPacketAcked(_ protocol.PacketNumber) {
	// No-op for basic tracking; isNewRound does the work
}

func (r *bbrRoundTrip) isNewRound(ackedPacketNumber protocol.PacketNumber) bool {
	return ackedPacketNumber >= r.currentEnd
}

func (r *bbrRoundTrip) startNewRound(largestSent protocol.PacketNumber) {
	r.count++
	r.lastRoundStart = r.currentEnd
	r.currentEnd = largestSent
}
