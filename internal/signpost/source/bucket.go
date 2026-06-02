package source

import (
	"crypto/sha256"
	"encoding/hex"
	"math"
	"sync"
	"time"
)

// Bucket is a continuous-fill token bucket. Capacity equals one hour's
// worth of tokens at the configured per-hour rate.
type Bucket struct {
	capacity float64
	perSec   float64

	mu           sync.Mutex
	tokens       float64
	lastFill     time.Time
	blockedUntil time.Time // reactive cap from GitHub headers; zero = unblocked
}

func newBucket(perHour int) *Bucket {
	if perHour <= 0 {
		panic("bucket rate must be > 0")
	}
	cap := float64(perHour)
	return &Bucket{
		capacity: cap,
		perSec:   cap / 3600.0,
		tokens:   cap, // start full
		lastFill: time.Now(),
	}
}

// TryTake atomically removes one token if available and the bucket is not
// reactively blocked. Returns false + the suggested wait until the next
// available token (or the unblock time, whichever is later).
func (b *Bucket) TryTake() (bool, time.Duration) {
	b.mu.Lock()
	defer b.mu.Unlock()

	now := time.Now()
	if !b.blockedUntil.IsZero() && now.Before(b.blockedUntil) {
		return false, b.blockedUntil.Sub(now)
	}

	elapsed := now.Sub(b.lastFill).Seconds()
	if elapsed > 0 {
		b.tokens = math.Min(b.capacity, b.tokens+elapsed*b.perSec)
		b.lastFill = now
	}
	if b.tokens < 1 {
		wait := time.Duration((1 - b.tokens) / b.perSec * float64(time.Second))
		return false, wait
	}
	b.tokens--
	return true, 0
}

// BlockUntil pushes the reactive unblock time forward (never backward).
func (b *Bucket) BlockUntil(t time.Time) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if t.After(b.blockedUntil) {
		b.blockedUntil = t
	}
}

// Registry routes per-credential traffic to a shared Bucket. Callers
// look up by token bytes; the empty bucket key is for unauthenticated.
type Registry struct {
	unauthRate int
	authRate   int

	mu      sync.Mutex
	buckets map[string]*Bucket // key: "" or sha256(token)[:16] hex
}

func NewRegistry(unauthPerHour, authPerHour int) *Registry {
	return &Registry{
		unauthRate: unauthPerHour,
		authRate:   authPerHour,
		buckets:    map[string]*Bucket{},
	}
}

// BucketFor returns the bucket for the given token bytes; nil/empty token
// resolves to the shared unauthenticated bucket. Buckets are created lazily.
func (r *Registry) BucketFor(token []byte) *Bucket {
	key, rate := r.keyFor(token)
	r.mu.Lock()
	defer r.mu.Unlock()
	b, ok := r.buckets[key]
	if !ok {
		b = newBucket(rate)
		r.buckets[key] = b
	}
	return b
}

func (r *Registry) keyFor(token []byte) (string, int) {
	if len(token) == 0 {
		return "", r.unauthRate
	}
	sum := sha256.Sum256(token)
	return hex.EncodeToString(sum[:8]), r.authRate
}

// CredentialID returns the bucket key used for log lines. Stable across
// process lifetime, never reveals the plaintext token.
func (r *Registry) CredentialID(token []byte) string {
	if len(token) == 0 {
		return "unauthenticated"
	}
	sum := sha256.Sum256(token)
	return "credhash:" + hex.EncodeToString(sum[:8])
}
