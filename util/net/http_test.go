package net

import (
	"fmt"
	"net/http"
	"net/netip"
	"strings"
	"testing"

	"github.com/alkaid/pitaya/v2/logger"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
	"go.uber.org/zap/zaptest/observer"
)

func TestGetClientIPDefaultsToSocketPeer(t *testing.T) {
	req := requestWithForwardedIPs(
		"203.0.113.10:4321",
		"198.51.100.1, 198.51.100.2",
		"198.51.100.3",
	)

	require.Equal(t, netip.MustParseAddr("203.0.113.10"), GetClientIP(req))
	require.Equal(t, []netip.Addr{netip.MustParseAddr("203.0.113.10")}, SourceIPs(req))
}

func TestGetClientIPWithTrustedProxies(t *testing.T) {
	trustedIPv4 := mustTrustedProxies(t, "10.0.0.0/8")
	trustedDualStack := mustTrustedProxies(t, "10.0.0.0/8", "2001:db8:ffff::/48")

	tests := []struct {
		name             string
		remoteAddr       string
		forwardedFor     string
		realIP           string
		trusted          TrustedProxies
		expectedClientIP string
	}{
		{
			name:             "single trusted proxy",
			remoteAddr:       "10.0.0.2:443",
			forwardedFor:     "198.51.100.7",
			trusted:          trustedIPv4,
			expectedClientIP: "198.51.100.7",
		},
		{
			name:             "rightmost untrusted hop is authoritative",
			remoteAddr:       "10.0.0.2:443",
			forwardedFor:     "198.51.100.7, 203.0.113.9, 10.0.0.1",
			trusted:          trustedIPv4,
			expectedClientIP: "203.0.113.9",
		},
		{
			name:             "all proxy hops trusted",
			remoteAddr:       "10.0.0.2:443",
			forwardedFor:     "198.51.100.7, 10.0.0.1",
			trusted:          trustedIPv4,
			expectedClientIP: "198.51.100.7",
		},
		{
			name:             "IPv6 trusted proxy",
			remoteAddr:       "[2001:db8:ffff::2]:443",
			forwardedFor:     "2001:db8:1::5",
			trusted:          trustedDualStack,
			expectedClientIP: "2001:db8:1::5",
		},
		{
			name:             "IPv4-mapped IPv6 is normalized",
			remoteAddr:       "[::ffff:10.0.0.2]:443",
			forwardedFor:     "::ffff:198.51.100.7",
			trusted:          trustedIPv4,
			expectedClientIP: "198.51.100.7",
		},
		{
			name:             "X-Real-IP fallback",
			remoteAddr:       "10.0.0.2:443",
			realIP:           "198.51.100.8",
			trusted:          trustedIPv4,
			expectedClientIP: "198.51.100.8",
		},
		{
			name:             "X-Forwarded-For takes precedence",
			remoteAddr:       "10.0.0.2:443",
			forwardedFor:     "198.51.100.7",
			realIP:           "198.51.100.8",
			trusted:          trustedIPv4,
			expectedClientIP: "198.51.100.7",
		},
		{
			name:             "plain IP remote address",
			remoteAddr:       "10.0.0.2",
			forwardedFor:     "198.51.100.7",
			trusted:          trustedIPv4,
			expectedClientIP: "198.51.100.7",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			req := requestWithForwardedIPs(tt.remoteAddr, tt.forwardedFor, tt.realIP)
			require.Equal(
				t,
				netip.MustParseAddr(tt.expectedClientIP),
				GetClientIPWithTrustedProxies(req, tt.trusted),
			)
		})
	}
}

func TestForwardedIPAddressLimitIsInclusive(t *testing.T) {
	trusted := mustTrustedProxies(t, "10.0.0.0/8")
	req := requestWithForwardedIPs(
		"10.0.0.2:443",
		forwardedAddresses(maxForwardedForAddresses),
		"",
	)

	require.Equal(
		t,
		netip.MustParseAddr("192.0.2.32"),
		GetClientIPWithTrustedProxies(req, trusted),
	)
}

func TestSourceIPsPreservesMultipleForwardedHeaderOrder(t *testing.T) {
	trusted := mustTrustedProxies(t, "10.0.0.0/8")
	req := requestWithForwardedIPs("10.0.0.2:443", "", "")
	req.Header.Add("X-Forwarded-For", "198.51.100.7")
	req.Header.Add("X-Forwarded-For", "10.0.0.1")

	require.Equal(t, []netip.Addr{
		netip.MustParseAddr("198.51.100.7"),
		netip.MustParseAddr("10.0.0.1"),
		netip.MustParseAddr("10.0.0.2"),
	}, sourceIPsWithTrustedProxies(req, trusted))
}

func TestSourceIPsDropsAssertionsLeftOfFirstUntrustedHop(t *testing.T) {
	trusted := mustTrustedProxies(t, "10.0.0.0/8")
	req := requestWithForwardedIPs(
		"10.0.0.2:443",
		"198.51.100.7, 203.0.113.9, 10.0.0.1",
		"",
	)

	require.Equal(t, []netip.Addr{
		netip.MustParseAddr("203.0.113.9"),
		netip.MustParseAddr("10.0.0.1"),
		netip.MustParseAddr("10.0.0.2"),
	}, sourceIPsWithTrustedProxies(req, trusted))
}

func TestForwardedIPFailuresFallBackAndLogOnce(t *testing.T) {
	core, observed := observer.New(zapcore.WarnLevel)
	previousLogger := logger.Zap
	logger.Zap = zap.New(core)
	t.Cleanup(func() { logger.Zap = previousLogger })

	trusted := mustTrustedProxies(t, "10.0.0.0/8")
	peer := netip.MustParseAddr("10.0.0.2")

	tests := []struct {
		name         string
		forwardedFor string
	}{
		{name: "multiple invalid tokens", forwardedFor: "not-an-ip, still-not-an-ip"},
		{name: "header too large", forwardedFor: strings.Repeat("1", maxForwardedForHeaderBytes+1)},
		{name: "too many addresses", forwardedFor: forwardedAddresses(maxForwardedForAddresses + 1)},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			observed.TakeAll()
			req := requestWithForwardedIPs("10.0.0.2:443", tt.forwardedFor, "")

			require.Equal(t, peer, GetClientIPWithTrustedProxies(req, trusted))
			entries := observed.TakeAll()
			require.Len(t, entries, 1)
			require.Equal(t, "ignored forwarded client IP headers", entries[0].Message)
		})
	}
}

func TestUntrustedPeerHeadersAreIgnoredWithoutParsingOrLogging(t *testing.T) {
	core, observed := observer.New(zapcore.WarnLevel)
	previousLogger := logger.Zap
	logger.Zap = zap.New(core)
	t.Cleanup(func() { logger.Zap = previousLogger })

	trusted := mustTrustedProxies(t, "10.0.0.0/8")
	req := requestWithForwardedIPs("203.0.113.10:443", "not-an-ip, still-not-an-ip", "also-invalid")

	require.Equal(t, netip.MustParseAddr("203.0.113.10"), GetClientIPWithTrustedProxies(req, trusted))
	require.Empty(t, observed.All())
}

func TestGetClientIPRejectsInvalidRemoteAddress(t *testing.T) {
	req := requestWithForwardedIPs("not-a-remote-address", "198.51.100.7", "")

	require.False(t, GetClientIPWithTrustedProxies(req, mustTrustedProxies(t, "0.0.0.0/0")).IsValid())
	require.Empty(t, SourceIPs(req))
}

func TestParseTrustedProxiesRejectsInvalidCIDR(t *testing.T) {
	_, err := ParseTrustedProxies([]string{"10.0.0.0/8", "not-a-cidr"})
	require.ErrorContains(t, err, `trusted proxy CIDR 1 "not-a-cidr"`)
}

func TestParseTrustedProxiesNormalizesIPv4MappedCIDR(t *testing.T) {
	trusted, err := ParseTrustedProxies([]string{"::ffff:10.0.0.0/104"})
	require.NoError(t, err)
	require.True(t, trusted.Contains(netip.MustParseAddr("10.1.2.3")))
	require.True(t, trusted.Contains(netip.MustParseAddr("::ffff:10.1.2.3")))

	_, err = ParseTrustedProxies([]string{"::ffff:0:0/80"})
	require.ErrorContains(t, err, "prefix length must be at least 96")
}

func FuzzGetClientIPWithTrustedProxies(f *testing.F) {
	previousLogger := logger.Zap
	logger.Zap = zap.NewNop()
	f.Cleanup(func() { logger.Zap = previousLogger })

	trusted, err := ParseTrustedProxies([]string{"10.0.0.0/8", "2001:db8:ffff::/48"})
	require.NoError(f, err)
	f.Add("10.0.0.2:443", "198.51.100.7, 10.0.0.1", "")
	f.Add("[2001:db8:ffff::2]:443", "2001:db8:1::5", "")
	f.Add("203.0.113.10", "not-an-ip", "198.51.100.8")
	f.Add("not-a-remote-address", strings.Repeat("1", maxForwardedForHeaderBytes+1), "")

	f.Fuzz(func(t *testing.T, remoteAddr, forwardedFor, realIP string) {
		req := requestWithForwardedIPs(remoteAddr, forwardedFor, realIP)
		sources := sourceIPsWithTrustedProxies(req, trusted)
		if len(sources) > maxForwardedForAddresses+1 {
			t.Fatalf("source IP count %d exceeds bound", len(sources))
		}

		clientIP := GetClientIPWithTrustedProxies(req, trusted)
		if len(sources) == 0 {
			if clientIP.IsValid() {
				t.Fatalf("invalid source chain returned client IP %s", clientIP)
			}
			return
		}
		if clientIP != sources[0] {
			t.Fatalf("client IP %s does not match authoritative source %s", clientIP, sources[0])
		}
	})
}

func requestWithForwardedIPs(remoteAddr, forwardedFor, realIP string) *http.Request {
	req := &http.Request{RemoteAddr: remoteAddr, Header: make(http.Header)}
	if forwardedFor != "" {
		req.Header.Set("X-Forwarded-For", forwardedFor)
	}
	if realIP != "" {
		req.Header.Set("X-Real-IP", realIP)
	}
	return req
}

func mustTrustedProxies(t *testing.T, cidrs ...string) TrustedProxies {
	t.Helper()
	trusted, err := ParseTrustedProxies(cidrs)
	require.NoError(t, err)
	return trusted
}

func forwardedAddresses(count int) string {
	parts := make([]string, count)
	for i := range parts {
		parts[i] = fmt.Sprintf("192.0.2.%d", i+1)
	}
	return strings.Join(parts, ",")
}
