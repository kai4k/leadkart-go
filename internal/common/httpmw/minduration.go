package httpmw

import (
	"bytes"
	"context"
	"net/http"
	"time"
)

// Pacer is the time source of [MinDurationWithPacer]: Now reads the clock, Wait
// blocks for d or until ctx ends. The zero value is not usable; production code
// calls [MinDuration], tests inject a recording clock instead of sleeping.
type Pacer struct {
	Now  func() time.Time
	Wait func(ctx context.Context, d time.Duration)
}

func realPacer() Pacer {
	return Pacer{
		Now: time.Now,
		Wait: func(ctx context.Context, d time.Duration) {
			timer := time.NewTimer(d)
			defer timer.Stop()
			select {
			case <-timer.C:
			case <-ctx.Done():
			}
		},
	}
}

// MinDuration buffers the response and releases it no earlier than floor after
// the handler started, so branch-dependent work ("email exists: persist a reset
// token" vs "unknown email: return") is not observable by response time (ADR
// 0044; OWASP Forgot Password Cheat Sheet "consistent response time"). The floor
// must exceed the slowest expected branch; a handler already slower than the
// floor is not delayed further. Client cancellation ends the wait. Use only on
// small-bodied routes: the whole response is held in memory.
func MinDuration(floor time.Duration) Middleware {
	return MinDurationWithPacer(floor, realPacer())
}

// MinDurationWithPacer is [MinDuration] over an injected [Pacer].
func MinDurationWithPacer(floor time.Duration, pacer Pacer) Middleware {
	if floor <= 0 {
		panic("httpmw: MinDuration floor must be positive")
	}
	if pacer.Now == nil || pacer.Wait == nil {
		panic("httpmw: MinDuration pacer needs Now and Wait")
	}
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			start := pacer.Now()
			buf := &bufferedResponse{header: http.Header{}}
			next.ServeHTTP(buf, r)
			if wait := floor - pacer.Now().Sub(start); wait > 0 {
				pacer.Wait(r.Context(), wait)
			}
			for k, v := range buf.header {
				w.Header()[k] = v
			}
			if buf.status == 0 {
				buf.status = http.StatusOK
			}
			w.WriteHeader(buf.status)
			_, _ = w.Write(buf.body.Bytes())
		})
	}
}

// bufferedResponse holds one handler's response until the floor has passed.
type bufferedResponse struct {
	header http.Header
	status int
	body   bytes.Buffer
}

func (b *bufferedResponse) Header() http.Header { return b.header }

func (b *bufferedResponse) WriteHeader(s int) {
	if b.status == 0 {
		b.status = s
	}
}

func (b *bufferedResponse) Write(p []byte) (int, error) {
	if b.status == 0 {
		b.status = http.StatusOK
	}
	return b.body.Write(p)
}
