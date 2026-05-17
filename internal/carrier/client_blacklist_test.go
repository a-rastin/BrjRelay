package carrier

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/kianmhz/GooseRelayVPN/internal/frame"
)

// TestEndpointFullRecoveryFromHighFailCount: a single successful response must
// fully clear failCount and blacklistedTill, regardless of how badly the
// endpoint was previously failing. This is the load-bearing invariant for
// post-quota-reset recovery: once Apps Script returns one valid 200, we go
// back to healthy.
func TestEndpointFullRecoveryFromHighFailCount(t *testing.T) {
	c, err := New(Config{
		ScriptURLs: []string{"https://example.invalid/exec"},
		AESKeyHex:  testKeyHex,
	})
	if err != nil {
		t.Fatalf("new client: %v", err)
	}

	// Simulate a long quota outage: hammer the endpoint with 403s, then a few
	// generic failures during the still-broken probe window.
	for i := 0; i < 50; i++ {
		c.markEndpoint403(0)
	}
	for i := 0; i < 20; i++ {
		c.markEndpointFailure(0)
	}

	c.endpointMu.Lock()
	failBefore := c.endpoints[0].failCount
	blBefore := c.endpoints[0].blacklistedTill
	c.endpointMu.Unlock()
	if failBefore == 0 {
		t.Fatalf("expected failCount > 0 after failures, got 0")
	}
	if !blBefore.After(time.Now()) {
		t.Fatalf("expected blacklistedTill in the future, got %v", blBefore)
	}

	c.markEndpointSuccess(0)

	c.endpointMu.Lock()
	failAfter := c.endpoints[0].failCount
	blAfter := c.endpoints[0].blacklistedTill
	c.endpointMu.Unlock()

	if failAfter != 0 {
		t.Fatalf("failCount not reset on success: got %d, want 0", failAfter)
	}
	if !blAfter.IsZero() {
		t.Fatalf("blacklistedTill not cleared on success: got %v, want zero", blAfter)
	}
}

// TestBlacklistTTLBoundedByMax: repeated failures must not push blacklistedTill
// arbitrarily far into the future. The TTL ramp tops out at
// endpointBlacklistMaxTTL (1h). If it weren't capped, a long outage could
// schedule recovery many hours past the actual quota reset.
func TestBlacklistTTLBoundedByMax(t *testing.T) {
	c, err := New(Config{
		ScriptURLs: []string{"https://example.invalid/exec"},
		AESKeyHex:  testKeyHex,
	})
	if err != nil {
		t.Fatalf("new client: %v", err)
	}

	for i := 0; i < 1000; i++ {
		c.markEndpoint403(0)
	}

	c.endpointMu.Lock()
	bl := c.endpoints[0].blacklistedTill
	c.endpointMu.Unlock()

	now := time.Now()
	maxAcceptable := now.Add(endpointBlacklistMaxTTL + 5*time.Second)
	if bl.After(maxAcceptable) {
		t.Fatalf("blacklistedTill grew past the documented cap: got %v, max %v (cap %s)",
			bl, maxAcceptable, endpointBlacklistMaxTTL)
	}
	if !bl.After(now.Add(endpointBlacklistMaxTTL - 5*time.Second)) {
		t.Fatalf("after many 403s, expected TTL near the 1h cap; got %v (now=%v)", bl, now)
	}
}

// TestPickRelayEndpointAllBlacklistedFallback: when every endpoint is
// blacklisted, pickRelayEndpoint must still return a usable index — and it
// must pick the one closest to expiry, not e.g. always index 0. This is the
// "fallback" branch used to keep the carrier responsive during a full outage.
func TestPickRelayEndpointAllBlacklistedFallback(t *testing.T) {
	c, err := New(Config{
		ScriptURLs: []string{
			"https://a.invalid/exec",
			"https://b.invalid/exec",
			"https://c.invalid/exec",
		},
		AESKeyHex: testKeyHex,
	})
	if err != nil {
		t.Fatalf("new client: %v", err)
	}

	now := time.Now()
	c.endpointMu.Lock()
	c.endpoints[0].blacklistedTill = now.Add(45 * time.Minute)
	c.endpoints[1].blacklistedTill = now.Add(10 * time.Minute) // soonest
	c.endpoints[2].blacklistedTill = now.Add(60 * time.Minute)
	c.endpointMu.Unlock()

	idx, _ := c.pickRelayEndpoint()
	if idx != 1 {
		t.Fatalf("fallback should pick soonest-to-expire endpoint (1); got %d", idx)
	}

	c.endpointMu.Lock()
	for i := range c.endpoints {
		if c.endpoints[i].blacklistedTill.Equal(now.Add(45*time.Minute)) && i == 0 {
			continue // ok
		}
	}
	gotBL := c.endpoints[1].blacklistedTill
	c.endpointMu.Unlock()
	if gotBL.Sub(now) < 9*time.Minute {
		t.Fatalf("pickRelayEndpoint mutated blacklistedTill of the picked endpoint: %v", gotBL)
	}
}

// blacklistHammerServer counts hits and returns either 403 (during the
// "outage") or echoes the batch (after the outage ends). It also tracks how
// many decoded frames it has seen after the outage ended, which is the signal
// for whether the carrier retransmitted dropped frames.
type blacklistHammerServer struct {
	t                  *testing.T
	aead               *frame.Crypto
	hits               atomic.Int64
	outage             atomic.Bool
	framesAfterOutage  atomic.Int64
	rxSeqMu            sync.Mutex
	rxSeq              map[[frame.SessionIDLen]byte]uint64
}

func newBlacklistHammerServer(t *testing.T, aead *frame.Crypto) *blacklistHammerServer {
	return &blacklistHammerServer{t: t, aead: aead, rxSeq: map[[frame.SessionIDLen]byte]uint64{}}
}

func (s *blacklistHammerServer) handler() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		s.hits.Add(1)
		if s.outage.Load() {
			w.WriteHeader(http.StatusForbidden)
			_, _ = w.Write([]byte("quota exhausted"))
			return
		}
		body, _ := io.ReadAll(r.Body)
		clientID, in, err := frame.DecodeBatch(s.aead, body)
		if err != nil {
			s.t.Errorf("decode: %v", err)
			w.WriteHeader(500)
			return
		}
		s.framesAfterOutage.Add(int64(len(in)))
		s.rxSeqMu.Lock()
		out := make([]*frame.Frame, 0, len(in))
		for _, f := range in {
			seq := s.rxSeq[f.SessionID]
			s.rxSeq[f.SessionID] = seq + 1
			out = append(out, &frame.Frame{
				SessionID: f.SessionID,
				Seq:       seq,
				Payload:   f.Payload,
			})
		}
		s.rxSeqMu.Unlock()
		resp, _ := frame.EncodeBatch(s.aead, clientID, out)
		w.Header().Set("Content-Type", "text/plain")
		_, _ = w.Write(resp)
	}
}

// TestSinglePostOutageSuccessRestoresTraffic: integration test. The relay
// returns 403 for a short window, then starts echoing. The carrier must
// recover and deliver an echoed payload promptly (within the fallback-probe
// rate of the all-blacklisted branch). This is the behaviour that determines
// whether users actually see traffic flow after Apps Script's midnight Pacific
// quota reset.
func TestSinglePostOutageSuccessRestoresTraffic(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test; skipped under -short")
	}
	aead, err := frame.NewCryptoFromHexKey(testKeyHex)
	if err != nil {
		t.Fatalf("crypto: %v", err)
	}
	bs := newBlacklistHammerServer(t, aead)
	bs.outage.Store(true)
	srv := httptest.NewServer(bs.handler())
	defer srv.Close()

	c, err := New(Config{ScriptURLs: []string{srv.URL}, AESKeyHex: testKeyHex})
	if err != nil {
		t.Fatalf("new client: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan struct{})
	go func() {
		_ = c.Run(ctx)
		close(done)
	}()

	s := c.NewSession("example.com:80")
	s.EnqueueTx([]byte("hello"))

	// Stay in outage for 2s so the endpoint gets blacklisted at the 5-min tier.
	time.Sleep(2 * time.Second)
	hitsDuringOutage := bs.hits.Load()
	bs.outage.Store(false)

	select {
	case got := <-s.RxChan:
		if string(got) != "hello" {
			t.Fatalf("got %q want %q", got, "hello")
		}
	case <-time.After(10 * time.Second):
		t.Fatalf("no echo received after outage ended.\n"+
			"  hits during outage:     %d (server returned 403)\n"+
			"  total hits at giveup:   %d\n"+
			"  frames decoded after outage ended: %d  (==0 indicates carrier sent only empty polls — SYN/payload was not retransmitted after the 403 drop)",
			hitsDuringOutage, bs.hits.Load(), bs.framesAfterOutage.Load())
	}

	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Run() did not return after cancel")
	}

	t.Logf("hits during 2s outage: %d, total hits at recovery: %d",
		hitsDuringOutage, bs.hits.Load())
}
