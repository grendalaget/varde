package mesh

import (
	"net/http"
	"sync"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/collectors"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

type metrics struct {
	reg              *prometheus.Registry
	peerRTT          *prometheus.GaugeVec
	peerBytesTx      *prometheus.GaugeVec
	peerBytesRx      *prometheus.GaugeVec
	peerPath         *prometheus.GaugeVec
	relayRTT         *prometheus.GaugeVec
	relayRegistered  *prometheus.GaugeVec
	datagramsDropped prometheus.Counter

	// labels written by the last collect, so removed peers/relays get
	// their series deleted instead of reporting stale values forever
	mu         sync.Mutex
	seenPeers  map[string]struct{}
	seenRelays map[string]struct{}
}

func newMetrics(reg *prometheus.Registry, n *Node) *metrics {
	if reg == nil {
		reg = prometheus.NewRegistry()
	}
	m := &metrics{
		reg: reg,
		peerRTT: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Name: "mesh_peer_rtt_us"}, []string{"peer"}),
		peerBytesTx: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Name: "mesh_peer_bytes_tx"}, []string{"peer"}),
		peerBytesRx: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Name: "mesh_peer_bytes_rx"}, []string{"peer"}),
		peerPath: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Name: "mesh_peer_path"}, []string{"peer", "kind"}),
		relayRTT: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Name: "mesh_relay_rtt_us"}, []string{"relay"}),
		relayRegistered: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Name: "mesh_relay_registered"}, []string{"relay"}),
		datagramsDropped: prometheus.NewCounter(prometheus.CounterOpts{
			Name: "mesh_datagrams_dropped_total"}),
		seenPeers:  map[string]struct{}{},
		seenRelays: map[string]struct{}{},
	}
	reg.MustRegister(m.peerRTT, m.peerBytesTx, m.peerBytesRx, m.peerPath,
		m.relayRTT, m.relayRegistered, m.datagramsDropped, collectors.NewGoCollector())
	return m
}

// collect snapshots node counters into the registry (called by /metrics via a
// collector would be fancier; pull-based refresh is fine).
func (m *metrics) collect(n *Node) {
	// serializes scrapes: the delete+set pairs below must stay atomic
	m.mu.Lock()
	defer m.mu.Unlock()
	n.mu.RLock()
	defer n.mu.RUnlock()
	peers := map[string]struct{}{}
	for _, p := range n.peers {
		peers[p.id] = struct{}{}
		st := p.state()
		m.peerRTT.WithLabelValues(p.id).Set(float64(st.GetRttUs()))
		// a peer keeps only its current path kind — clear the previous one
		m.peerPath.DeletePartialMatch(prometheus.Labels{"peer": p.id})
		m.peerPath.WithLabelValues(p.id, st.GetPath().String()).Set(1)
		m.peerBytesTx.WithLabelValues(p.id).Set(float64(st.GetBytesTx()))
		m.peerBytesRx.WithLabelValues(p.id).Set(float64(st.GetBytesRx()))
	}
	relays := map[string]struct{}{}
	for id, rc := range n.relays {
		relays[id] = struct{}{}
		st, _ := rc.state()
		m.relayRTT.WithLabelValues(id).Set(float64(st.GetRttUs()))
		if st.GetRegistered() {
			m.relayRegistered.WithLabelValues(id).Set(1)
		} else {
			m.relayRegistered.WithLabelValues(id).Set(0)
		}
	}

	for id := range m.seenPeers {
		if _, ok := peers[id]; ok {
			continue
		}
		m.peerRTT.DeletePartialMatch(prometheus.Labels{"peer": id})
		m.peerPath.DeletePartialMatch(prometheus.Labels{"peer": id})
		m.peerBytesTx.DeleteLabelValues(id)
		m.peerBytesRx.DeleteLabelValues(id)
	}
	for id := range m.seenRelays {
		if _, ok := relays[id]; ok {
			continue
		}
		m.relayRTT.DeleteLabelValues(id)
		m.relayRegistered.DeleteLabelValues(id)
	}
	m.seenPeers = peers
	m.seenRelays = relays
}

func (m *metrics) handler(n *Node) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		m.collect(n)
		promhttp.HandlerFor(m.reg, promhttp.HandlerOpts{}).ServeHTTP(w, r)
	})
}
