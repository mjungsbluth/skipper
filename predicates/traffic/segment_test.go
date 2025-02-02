package traffic_test

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/zalando/skipper/eskip"
	"github.com/zalando/skipper/filters/builtin"
	"github.com/zalando/skipper/predicates"
	"github.com/zalando/skipper/predicates/primitive"
	"github.com/zalando/skipper/predicates/tee"
	"github.com/zalando/skipper/predicates/traffic"
	"github.com/zalando/skipper/proxy/proxytest"
	"github.com/zalando/skipper/routing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestTrafficSegmentInvalidCreateArguments(t *testing.T) {
	spec := traffic.NewSegment()

	for _, def := range []string{
		`TrafficSegment()`,
		`TrafficSegment(1)`,
		`TrafficSegment(1, 0)`,
		`TrafficSegment(0, 1.1)`,
		`TrafficSegment(1, 2)`,
		`TrafficSegment(0, "1")`,
		`TrafficSegment("0", 1)`,
	} {
		t.Run(def, func(t *testing.T) {
			pp := eskip.MustParsePredicates(def)
			require.Len(t, pp, 1)

			_, err := spec.Create(pp[0].Args)
			assert.Error(t, err)
		})
	}
}

func requestWithR(r float64) *http.Request {
	req := &http.Request{}
	req = req.WithContext(routing.NewContext(req.Context()))

	_ = routing.FromContext(req.Context(), traffic.ExportRandomValue, func() float64 { return r })
	return req
}

func getN(t *testing.T, client *proxytest.TestClient, url string, n int) map[int]int {
	codes := make(map[int]int)
	for i := 0; i < n; i++ {
		rsp, err := client.Get(url)
		require.NoError(t, err)
		rsp.Body.Close()

		codes[rsp.StatusCode]++
	}
	return codes
}

func TestTrafficSegmentMatch(t *testing.T) {
	pp := eskip.MustParsePredicates(`TrafficSegment(0, 0.5)`)
	require.Len(t, pp, 1)

	spec := traffic.NewSegment()
	p, err := spec.Create(pp[0].Args)
	require.NoError(t, err)

	assert.True(t, p.Match(requestWithR(0.0)))
	assert.True(t, p.Match(requestWithR(0.1)))
	assert.True(t, p.Match(requestWithR(0.49)))

	assert.False(t, p.Match(requestWithR(0.5))) // upper interval boundary is excluded
	assert.False(t, p.Match(requestWithR(0.6)))
	assert.False(t, p.Match(requestWithR(1.0)))
}

func TestTrafficSegmentMinEqualsMax(t *testing.T) {
	pp := eskip.MustParsePredicates(`TrafficSegment(0.5, 0.5)`)
	require.Len(t, pp, 1)

	spec := traffic.NewSegment()
	p, err := spec.Create(pp[0].Args)
	require.NoError(t, err)

	assert.False(t, p.Match(requestWithR(0.0)))
	assert.False(t, p.Match(requestWithR(0.5)))
	assert.False(t, p.Match(requestWithR(1.0)))
}

func TestTrafficSegmentSpec(t *testing.T) {
	spec := traffic.NewSegment()

	assert.Equal(t, predicates.TrafficSegmentName, spec.Name())
	assert.Equal(t, -1, spec.Weight())
}

func TestTrafficSegmentSplit(t *testing.T) {
	p := proxytest.Config{
		RoutingOptions: routing.Options{
			FilterRegistry: builtin.MakeRegistry(),
			Predicates: []routing.PredicateSpec{
				traffic.NewSegment(),
			},
		},
		Routes: eskip.MustParse(`
			r50: Path("/test") && TrafficSegment(0.0, 0.5) -> status(200) -> <shunt>;
			r30: Path("/test") && TrafficSegment(0.5, 0.8) -> status(201) -> <shunt>;
			r20: Path("/test") && TrafficSegment(0.8, 1.0) -> status(202) -> <shunt>;
		`),
	}.Create()
	defer p.Close()

	const (
		N     = 1_000
		delta = 0.05 * N
	)

	codes := getN(t, p.Client(), p.URL+"/test", N)

	t.Logf("Response codes: %v", codes)

	assert.InDelta(t, N*0.5, codes[200], delta)
	assert.InDelta(t, N*0.3, codes[201], delta)
	assert.InDelta(t, N*0.2, codes[202], delta)
}

func TestTrafficSegmentTeeLoopback(t *testing.T) {
	loopRequestsPtr := new(int32)
	loopBackend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(loopRequestsPtr, 1)
	}))
	defer loopBackend.Close()

	p := proxytest.Config{
		RoutingOptions: routing.Options{
			FilterRegistry: builtin.MakeRegistry(),
			Predicates: []routing.PredicateSpec{
				traffic.NewSegment(),
				tee.New(),
				primitive.NewTrue(),
			},
		},
		Routes: eskip.MustParse(fmt.Sprintf(`
			r0: * -> status(200) -> <shunt>;
			r1: Path("/test") && TrafficSegment(0.0, 0.5) -> teeLoopback("a-loop") -> status(201) -> <shunt>;
			r2: Path("/test") && Tee("a-loop") && True() -> "%s";
		`, loopBackend.URL)),
	}.Create()
	defer p.Close()

	const (
		N     = 1_000
		delta = 0.05 * N
	)

	codes := getN(t, p.Client(), p.URL+"/test", N)

	// wait for loopback requests to complete
	time.Sleep(100 * time.Millisecond)

	loopRequests := int(atomic.LoadInt32(loopRequestsPtr))

	t.Logf("Response codes: %v, loopRequests: %d", codes, loopRequests)

	assert.InDelta(t, N*0.5, codes[200], delta)
	assert.InDelta(t, N*0.5, codes[201], delta)
	assert.Equal(t, codes[201], loopRequests)
}

func TestTrafficSegmentLoopbackBackend(t *testing.T) {
	p := proxytest.Config{
		RoutingOptions: routing.Options{
			FilterRegistry: builtin.MakeRegistry(),
			Predicates: []routing.PredicateSpec{
				traffic.NewSegment(),
				tee.New(),
				primitive.NewTrue(),
			},
		},
		Routes: eskip.MustParse(`
			r0: * -> status(200) -> <shunt>;
			r1: Path("/test") && TrafficSegment(0.0, 0.5) -> setPath("a-loop") -> <loopback>;
			r2: Path("/a-loop") -> status(201) -> <shunt>;
		`),
	}.Create()
	defer p.Close()

	const (
		N     = 1_000
		delta = 0.05 * N
	)

	codes := getN(t, p.Client(), p.URL+"/test", N)

	t.Logf("Response codes: %v", codes)

	assert.InDelta(t, N*0.5, codes[200], delta)
	assert.InDelta(t, N*0.5, codes[201], delta)
}
