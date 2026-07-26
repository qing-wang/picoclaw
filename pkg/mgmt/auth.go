package mgmt

import (
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"net/http"
	"sync"
	"time"
)

// ---- pair rate limiter ------------------------------------------------------
// 5 pair requests per minute per source IP.

const (
	pairRateWindow        = time.Minute
	pairRateMax           = 5
	rateLimiterMaxEntries = 4096
)

type pairRateLimiter struct {
	mu          sync.Mutex
	entries     map[string]*pairEntry
	insertCount int
}

type pairEntry struct {
	count     int
	windowEnd time.Time
}

func newPairRateLimiter() *pairRateLimiter {
	return &pairRateLimiter{entries: make(map[string]*pairEntry)}
}

// allow returns true if the request from ip is within rate limit, and records it.
func (r *pairRateLimiter) allow(ip string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	now := time.Now()
	e, ok := r.entries[ip]
	if !ok || now.After(e.windowEnd) {
		// New or expired entry: sweep and check cap before inserting
		r.insertCount++
		if r.insertCount%100 == 0 {
			r.sweep(now)
		}
		if len(r.entries) >= rateLimiterMaxEntries {
			r.sweep(now)
			if len(r.entries) >= rateLimiterMaxEntries {
				return false // cap exceeded; protect existing state
			}
		}
		r.entries[ip] = &pairEntry{count: 1, windowEnd: now.Add(pairRateWindow)}
		return true
	}
	if e.count >= pairRateMax {
		return false
	}
	e.count++
	return true
}

// sweep removes expired entries from the rate limiter map. Must be called with mu held.
func (r *pairRateLimiter) sweep(now time.Time) {
	for ip, e := range r.entries {
		if now.After(e.windowEnd) {
			delete(r.entries, ip)
		}
	}
}

// ---- auth failure tracker ---------------------------------------------------
// 5 failed auth attempts per IP → 10-minute lockout.

const (
	authMaxFails          = 5
	authLockoutDur        = 10 * time.Minute
	authTrackerMaxEntries = 4096
)

type authTracker struct {
	mu          sync.Mutex
	entries     map[string]*authEntry
	insertCount int
}

type authEntry struct {
	fails       int
	lockedUntil time.Time
}

func newAuthTracker() *authTracker {
	return &authTracker{entries: make(map[string]*authEntry)}
}

// isLocked returns true if ip is currently locked out.
func (t *authTracker) isLocked(ip string) bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	e, ok := t.entries[ip]
	if !ok {
		return false
	}
	if !e.lockedUntil.IsZero() && time.Now().Before(e.lockedUntil) {
		return true
	}
	return false
}

// recordFailure increments the failure counter for ip, locking it out on authMaxFails.
func (t *authTracker) recordFailure(ip string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	now := time.Now()
	e, ok := t.entries[ip]
	if !ok {
		// New entry: sweep and check cap before inserting
		t.insertCount++
		if t.insertCount%100 == 0 {
			t.sweep(now)
		}
		if len(t.entries) >= authTrackerMaxEntries {
			t.sweep(now)
			if len(t.entries) >= authTrackerMaxEntries {
				return // cap exceeded; silently drop (existing lockouts preserved)
			}
		}
		e = &authEntry{}
		t.entries[ip] = e
	}
	// Reset fail count if lockout has expired
	if !e.lockedUntil.IsZero() && now.After(e.lockedUntil) {
		e.fails = 0
		e.lockedUntil = time.Time{}
	}
	e.fails++
	if e.fails >= authMaxFails {
		e.lockedUntil = now.Add(authLockoutDur)
	}
}

// recordSuccess resets the failure counter for ip.
func (t *authTracker) recordSuccess(ip string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	delete(t.entries, ip)
}

// sweep removes entries whose lockout has expired or that have no failures.
// Must be called with mu held.
func (t *authTracker) sweep(now time.Time) {
	for ip, e := range t.entries {
		if e.fails == 0 {
			delete(t.entries, ip)
		} else if !e.lockedUntil.IsZero() && now.After(e.lockedUntil) {
			delete(t.entries, ip)
		}
	}
}

// ---- withAuth middleware -----------------------------------------------------

// withAuth wraps a handler with Bearer token authentication.
// SHA-256(presented token) is compared against each paired_clients entry using
// constant-time comparison to prevent timing attacks.
func (s *Server) withAuth(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		ip := extractIP(r.RemoteAddr)

		if s.authTrk.isLocked(ip) {
			writeJSON(w, http.StatusTooManyRequests, map[string]string{"error": "too many attempts"})
			return
		}

		token := extractBearerToken(r.Header.Get("Authorization"))
		if token == "" {
			writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "unauthorized"})
			return
		}

		sum := sha256.Sum256([]byte(token))
		tokenHash := hex.EncodeToString(sum[:])

		cfg, err := s.loadConfig()
		if err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "config unavailable"})
			return
		}

		for _, c := range cfg.Mgmt.PairedClients {
			if subtle.ConstantTimeCompare([]byte(tokenHash), []byte(c.TokenSHA256)) == 1 {
				s.authTrk.recordSuccess(ip)
				next(w, r)
				return
			}
		}

		s.authTrk.recordFailure(ip)
		writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "unauthorized"})
	}
}
