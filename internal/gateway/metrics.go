package gateway

import (
	"fmt"
	"net/http"
	"sync/atomic"
)

type Metrics struct {
	active          atomic.Int64
	connections     atomic.Uint64
	durationNanos   atomic.Uint64
	durationCount   atomic.Uint64
	bytesFromClient atomic.Uint64
	bytesToClient   atomic.Uint64
	replayGaps      atomic.Uint64
	takeovers       atomic.Uint64
	failures        atomic.Uint64
}

func (m *Metrics) ServeHTTP(response http.ResponseWriter, _ *http.Request) {
	response.Header().Set("Content-Type", "text/plain; version=0.0.4")
	_, _ = fmt.Fprintf(response, `# TYPE sandherd_gateway_active_connections gauge
sandherd_gateway_active_connections %d
# TYPE sandherd_gateway_connections_total counter
sandherd_gateway_connections_total %d
# TYPE sandherd_gateway_connection_duration_seconds summary
sandherd_gateway_connection_duration_seconds_sum %g
sandherd_gateway_connection_duration_seconds_count %d
# TYPE sandherd_gateway_bytes_from_client_total counter
sandherd_gateway_bytes_from_client_total %d
# TYPE sandherd_gateway_bytes_to_client_total counter
sandherd_gateway_bytes_to_client_total %d
# TYPE sandherd_gateway_replay_gaps_total counter
sandherd_gateway_replay_gaps_total %d
# TYPE sandherd_gateway_takeovers_total counter
sandherd_gateway_takeovers_total %d
# TYPE sandherd_gateway_failures_total counter
sandherd_gateway_failures_total %d
`, m.active.Load(), m.connections.Load(), float64(m.durationNanos.Load())/1e9, m.durationCount.Load(), m.bytesFromClient.Load(), m.bytesToClient.Load(), m.replayGaps.Load(), m.takeovers.Load(), m.failures.Load())
}
