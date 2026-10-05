package main

import (
	"net/http"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/collectors"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

// promReg is the registry used for the optional --debug-listen metrics.
type promReg = prometheus.Registry

type metrics struct {
	reg              *promReg
	peerRTT          *prometheus.GaugeVec
	peerBytesTx      *prometheus.CounterVec
	peerBytesRx      *prometheus.CounterVec
	peerPath         *prometheus.GaugeVec
	relayRTT         *prometheus.GaugeVec
	relayRegistered  *prometheus.GaugeVec
	datagramsDropped prometheus.Counter
}

func newMetrics(reg *promReg, n *Node) *metrics {
	if reg == nil {
		reg = prometheus.NewRegistry()
	}
	m := &metrics{
		reg: reg,
		peerRTT: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Name: "mesh_peer_rtt_us"}, []string{"peer"}),
		peerBytesTx: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "mesh_peer_bytes_tx_total"}, []string{"peer"}),
		peerBytesRx: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "mesh_peer_bytes_rx_total"}, []string{"peer"}),
		peerPath: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Name: "mesh_peer_path"}, []string{"peer", "kind"}),
		relayRTT: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Name: "mesh_relay_rtt_us"}, []string{"relay"}),
		relayRegistered: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Name: "mesh_relay_registered"}, []string{"relay"}),
		datagramsDropped: prometheus.NewCounter(prometheus.CounterOpts{
			Name: "mesh_datagrams_dropped_total"}),
	}
	reg.MustRegister(m.peerRTT, m.peerBytesTx, m.peerBytesRx, m.peerPath,
		m.relayRTT, m.relayRegistered, m.datagramsDropped, collectors.NewGoCollector())
	return m
}

// collect snapshots node counters into the registry (called by /metrics via a
// collector would be fancier; pull-based refresh is fine).
func (m *metrics) collect(n *Node) {
	n.mu.RLock()
	defer n.mu.RUnlock()
	for _, p := range n.peers {
		st := p.state()
		m.peerRTT.WithLabelValues(p.id).Set(float64(st.GetRttUs()))
		m.peerPath.WithLabelValues(p.id, st.GetPath().String()).Set(1)
		m.peerBytesTx.WithLabelValues(p.id).Add(0) // ensure series exists
	}
	for id, rc := range n.relays {
		st, _ := rc.state()
		m.relayRTT.WithLabelValues(id).Set(float64(st.GetRttUs()))
		if st.GetRegistered() {
			m.relayRegistered.WithLabelValues(id).Set(1)
		} else {
			m.relayRegistered.WithLabelValues(id).Set(0)
		}
	}
}

func (m *metrics) handler(n *Node) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		m.collect(n)
		promhttp.HandlerFor(m.reg, promhttp.HandlerOpts{}).ServeHTTP(w, r)
	})
}
