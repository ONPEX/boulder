package bdns

import (
	"fmt"
	"math/rand"
	"net"
	"sync"
	"time"

	"github.com/jmhodges/clock"
	"github.com/miekg/dns"
	"github.com/prometheus/client_golang/prometheus"
	"golang.org/x/net/context"

	"github.com/letsencrypt/boulder/metrics"
)

func parseCidr(network string, comment string) net.IPNet {
	_, net, err := net.ParseCIDR(network)
	if err != nil {
		panic(fmt.Sprintf("error parsing %s (%s): %s", network, comment, err))
	}
	return *net
}

var (
	// Private CIDRs to ignore
	privateNetworks = []net.IPNet{
		// RFC1918
		// 10.0.0.0/8
		{
			IP:   []byte{10, 0, 0, 0},
			Mask: []byte{255, 0, 0, 0},
		},
		// 172.16.0.0/12
		{
			IP:   []byte{172, 16, 0, 0},
			Mask: []byte{255, 240, 0, 0},
		},
		// 192.168.0.0/16
		{
			IP:   []byte{192, 168, 0, 0},
			Mask: []byte{255, 255, 0, 0},
		},
		// RFC5735
		// 127.0.0.0/8
		{
			IP:   []byte{127, 0, 0, 0},
			Mask: []byte{255, 0, 0, 0},
		},
		// RFC1122 Section 3.2.1.3
		// 0.0.0.0/8
		{
			IP:   []byte{0, 0, 0, 0},
			Mask: []byte{255, 0, 0, 0},
		},
		// RFC3927
		// 169.254.0.0/16
		{
			IP:   []byte{169, 254, 0, 0},
			Mask: []byte{255, 255, 0, 0},
		},
		// RFC 5736
		// 192.0.0.0/24
		{
			IP:   []byte{192, 0, 0, 0},
			Mask: []byte{255, 255, 255, 0},
		},
		// RFC 5737
		// 192.0.2.0/24
		{
			IP:   []byte{192, 0, 2, 0},
			Mask: []byte{255, 255, 255, 0},
		},
		// 198.51.100.0/24
		{
			IP:   []byte{192, 51, 100, 0},
			Mask: []byte{255, 255, 255, 0},
		},
		// 203.0.113.0/24
		{
			IP:   []byte{203, 0, 113, 0},
			Mask: []byte{255, 255, 255, 0},
		},
		// RFC 3068
		// 192.88.99.0/24
		{
			IP:   []byte{192, 88, 99, 0},
			Mask: []byte{255, 255, 255, 0},
		},
		// RFC 2544
		// 192.18.0.0/15
		{
			IP:   []byte{192, 18, 0, 0},
			Mask: []byte{255, 254, 0, 0},
		},
		// RFC 3171
		// 224.0.0.0/4
		{
			IP:   []byte{224, 0, 0, 0},
			Mask: []byte{240, 0, 0, 0},
		},
		// RFC 1112
		// 240.0.0.0/4
		{
			IP:   []byte{240, 0, 0, 0},
			Mask: []byte{240, 0, 0, 0},
		},
		// RFC 919 Section 7
		// 255.255.255.255/32
		{
			IP:   []byte{255, 255, 255, 255},
			Mask: []byte{255, 255, 255, 255},
		},
		// RFC 6598
		// 100.64.0.0./10
		{
			IP:   []byte{100, 64, 0, 0},
			Mask: []byte{255, 192, 0, 0},
		},
	}
	// Sourced from https://www.iana.org/assignments/iana-ipv6-special-registry/iana-ipv6-special-registry.xhtml
	// where Global, Source, or Destination is False
	privateV6Networks = []net.IPNet{
		parseCidr("::/128", "RFC 4291: Unspecified Address"),
		parseCidr("::1/128", "RFC 4291: Loopback Address"),
		parseCidr("::ffff:0:0/96", "RFC 4291: IPv4-mapped Address"),
		parseCidr("100::/64", "RFC 6666: Discard Address Block"),
		parseCidr("2001::/23", "RFC 2928: IETF Protocol Assignments"),
		parseCidr("2001:2::/48", "RFC 5180: Benchmarking"),
		parseCidr("2001:db8::/32", "RFC 3849: Documentation"),
		parseCidr("2001::/32", "RFC 4380: TEREDO"),
		parseCidr("fc00::/7", "RFC 4193: Unique-Local"),
		parseCidr("fe80::/10", "RFC 4291: Section 2.5.6 Link-Scoped Unicast"),
		parseCidr("ff00::/8", "RFC 4291: Section 2.7"),
		// We disable validations to IPs under the 6to4 anycase prefix because
		// there's too much risk of a malicious actor advertising the prefix and
		// answering validations for a 6to4 host they do not control.
		// https://community.letsencrypt.org/t/problems-validating-ipv6-against-host-running-6to4/18312/9
		parseCidr("2002::/16", "RFC 7526: 6to4 anycast prefix deprecated"),
	}
)

// DNSClient queries for DNS records
type DNSClient interface {
	LookupTXT(context.Context, string) (txts []string, authorities []string, err error)
	LookupHost(context.Context, string) ([]net.IP, error)
	LookupCAA(context.Context, string) ([]*dns.CAA, error)
	LookupMX(context.Context, string) ([]string, error)
}

// DNSClientImpl represents a client that talks to an external resolver
type DNSClientImpl struct {
	dnsClient                exchanger
	servers                  []string
	allowRestrictedAddresses bool
	maxTries                 int
	clk                      clock.Clock

	queryTime             *prometheus.HistogramVec
	totalLookupTime       *prometheus.HistogramVec
	cancelCounter         *prometheus.CounterVec
	usedAllRetriesCounter *prometheus.CounterVec
}

var _ DNSClient = &DNSClientImpl{}

type exchanger interface {
	Exchange(m *dns.Msg, a string) (*dns.Msg, time.Duration, error)
}

// NewDNSClientImpl constructs a new DNS resolver object that utilizes the
// provided list of DNS servers for resolution.
func NewDNSClientImpl(
	readTimeout time.Duration,
	servers []string,
	stats metrics.Scope,
	clk clock.Clock,
	maxTries int,
) *DNSClientImpl {
	stats = stats.NewScope("DNS")
	// TODO(jmhodges): make constructor use an Option func pattern
	dnsClient := new(dns.Client)

	// Set timeout for underlying net.Conn
	dnsClient.ReadTimeout = readTimeout
	dnsClient.Net = "udp"

	queryTime := prometheus.NewHistogramVec(
		prometheus.HistogramOpts{
			Name: "dns_query_time",
			Help: "Time taken to perform a DNS query",
		},
		[]string{"qtype", "result", "authenticated_data"},
	)
	totalLookupTime := prometheus.NewHistogramVec(
		prometheus.HistogramOpts{
			Name: "dns_total_lookup_time",
			Help: "Time taken to perform a DNS lookup, including all retried queries",
		},
		[]string{"qtype", "result", "authenticated_data", "retries"},
	)
	cancelCounter := prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "dns_query_cancels",
			Help: "Counter of canceled DNS queries",
		},
		[]string{"qtype"},
	)
	usedAllRetriesCounter := prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "dns_query_used_all_retries",
			Help: "Counter of DNS queries which used all its retries",
		},
		[]string{"qtype"},
	)
	stats.MustRegister(queryTime, totalLookupTime, cancelCounter, usedAllRetriesCounter)

	return &DNSClientImpl{
		dnsClient:                dnsClient,
		servers:                  servers,
		allowRestrictedAddresses: false,
		maxTries:                 maxTries,
		clk:                      clk,
		queryTime:                queryTime,
		totalLookupTime:          totalLookupTime,
		cancelCounter:            cancelCounter,
		usedAllRetriesCounter:    usedAllRetriesCounter,
	}
}

// NewTestDNSClientImpl constructs a new DNS resolver object that utilizes the
// provided list of DNS servers for resolution and will allow loopback addresses.
// This constructor should *only* be called from tests (unit or integration).
func NewTestDNSClientImpl(readTimeout time.Duration, servers []string, stats metrics.Scope, clk clock.Clock, maxTries int) *DNSClientImpl {
	resolver := NewDNSClientImpl(readTimeout, servers, stats, clk, maxTries)
	resolver.allowRestrictedAddresses = true
	return resolver
}

// exchangeOne performs a single DNS exchange with a randomly chosen server
// out of the server list, returning the response, time, and error (if any).
// We assume that the upstream resolver requests and validates DNSSEC records
// itself.
func (dnsClient *DNSClientImpl) exchangeOne(ctx context.Context, hostname string, qtype uint16) (resp *dns.Msg, err error) {
	m := new(dns.Msg)
	// Set question type
	m.SetQuestion(dns.Fqdn(hostname), qtype)
	// Set the AD bit in the query header so that the resolver knows that
	// we are interested in this bit in the response header. If this isn't
	// set the AD bit in the response is useless (RFC 6840 Section 5.7).
	// This has no security implications, it simply allows us to gather
	// metrics about the percentage of responses that are secured with
	// DNSSEC.
	m.AuthenticatedData = true

	if len(dnsClient.servers) < 1 {
		return nil, fmt.Errorf("Not configured with at least one DNS Server")
	}

	// Randomly pick a server
	chosenServer := dnsClient.servers[rand.Intn(len(dnsClient.servers))]

	start := dnsClient.clk.Now()
	client := dnsClient.dnsClient
	qtypeStr := dns.TypeToString[qtype]
	tries := 1
	defer func() {
		result, authenticated := "failed", ""
		if resp != nil {
			result = dns.RcodeToString[resp.Rcode]
			authenticated = fmt.Sprintf("%t", resp.AuthenticatedData)
		}
		dnsClient.totalLookupTime.With(prometheus.Labels{
			"qtype":              qtypeStr,
			"result":             result,
			"authenticated_data": authenticated,
			"retries":            fmt.Sprintf("%d", tries),
		}).Observe(dnsClient.clk.Since(start).Seconds())
	}()
	for {
		ch := make(chan dnsResp, 1)

		go func() {
			rsp, rtt, err := client.Exchange(m, chosenServer)
			result, authenticated := "failed", ""
			if rsp != nil {
				result = dns.RcodeToString[rsp.Rcode]
				authenticated = fmt.Sprintf("%t", rsp.AuthenticatedData)
			}
			dnsClient.queryTime.With(prometheus.Labels{
				"qtype":              qtypeStr,
				"result":             result,
				"authenticated_data": authenticated,
			}).Observe(rtt.Seconds())
			ch <- dnsResp{m: rsp, err: err}
		}()
		select {
		case <-ctx.Done():
			dnsClient.cancelCounter.With(prometheus.Labels{"qtype": qtypeStr}).Inc()
			err = ctx.Err()
			return
		case r := <-ch:
			if r.err != nil {
				operr, ok := r.err.(*net.OpError)
				isRetryable := ok && operr.Temporary()
				hasRetriesLeft := tries < dnsClient.maxTries
				if isRetryable && hasRetriesLeft {
					tries++
					continue
				} else if isRetryable && !hasRetriesLeft {
					dnsClient.usedAllRetriesCounter.With(prometheus.Labels{"qtype": qtypeStr}).Inc()
				}
			}
			resp, err = r.m, r.err
			return
		}
	}

}

type dnsResp struct {
	m   *dns.Msg
	err error
}

// LookupTXT sends a DNS query to find all TXT records associated with
// the provided hostname which it returns along with the returned
// DNS authority section.
func (dnsClient *DNSClientImpl) LookupTXT(ctx context.Context, hostname string) ([]string, []string, error) {
	var txt []string
	dnsType := dns.TypeTXT
	r, err := dnsClient.exchangeOne(ctx, hostname, dnsType)
	if err != nil {
		return nil, nil, &DNSError{dnsType, hostname, err, -1}
	}
	if r.Rcode != dns.RcodeSuccess {
		return nil, nil, &DNSError{dnsType, hostname, nil, r.Rcode}
	}

	for _, answer := range r.Answer {
		if answer.Header().Rrtype == dnsType {
			if txtRec, ok := answer.(*dns.TXT); ok {
				txt = append(txt, txtRec.Txt...)
			}
		}
	}

	authorities := []string{}
	for _, a := range r.Ns {
		authorities = append(authorities, a.String())
	}

	return txt, authorities, err
}

func isPrivateV4(ip net.IP) bool {
	for _, net := range privateNetworks {
		if net.Contains(ip) {
			return true
		}
	}
	return false
}

func isPrivateV6(ip net.IP) bool {
	for _, net := range privateV6Networks {
		if net.Contains(ip) {
			return true
		}
	}
	return false
}

func (dnsClient *DNSClientImpl) lookupIP(ctx context.Context, hostname string, ipType uint16) ([]dns.RR, error) {
	resp, err := dnsClient.exchangeOne(ctx, hostname, ipType)
	if err != nil {
		return nil, &DNSError{ipType, hostname, err, -1}
	}
	if resp.Rcode != dns.RcodeSuccess {
		return nil, &DNSError{ipType, hostname, nil, resp.Rcode}
	}
	return resp.Answer, nil
}

// LookupHost sends a DNS query to find all A and AAAA records associated with
// the provided hostname. This method assumes that the external resolver will
// chase CNAME/DNAME aliases and return relevant records.  It will retry
// requests in the case of temporary network errors. It can return net package,
// context.Canceled, and context.DeadlineExceeded errors, all wrapped in the
// DNSError type.
func (dnsClient *DNSClientImpl) LookupHost(ctx context.Context, hostname string) ([]net.IP, error) {
	var recordsA, recordsAAAA []dns.RR
	var errA, errAAAA error
	var wg sync.WaitGroup

	wg.Add(1)
	go func() {
		defer wg.Done()
		recordsA, errA = dnsClient.lookupIP(ctx, hostname, dns.TypeA)
	}()
	wg.Add(1)
	go func() {
		defer wg.Done()
		recordsAAAA, errAAAA = dnsClient.lookupIP(ctx, hostname, dns.TypeAAAA)
	}()
	wg.Wait()

	if errA != nil && errAAAA != nil {
		return nil, errA
	}

	var addrs []net.IP

	for _, answer := range recordsA {
		if answer.Header().Rrtype == dns.TypeA {
			if a, ok := answer.(*dns.A); ok && a.A.To4() != nil && (!isPrivateV4(a.A) || dnsClient.allowRestrictedAddresses) {
				addrs = append(addrs, a.A)
			}
		}
	}
	for _, answer := range recordsAAAA {
		if answer.Header().Rrtype == dns.TypeAAAA {
			if aaaa, ok := answer.(*dns.AAAA); ok && aaaa.AAAA.To16() != nil && (!isPrivateV6(aaaa.AAAA) || dnsClient.allowRestrictedAddresses) {
				addrs = append(addrs, aaaa.AAAA)
			}
		}
	}

	return addrs, nil
}

// LookupCAA sends a DNS query to find all CAA records associated with
// the provided hostname.
func (dnsClient *DNSClientImpl) LookupCAA(ctx context.Context, hostname string) ([]*dns.CAA, error) {
	dnsType := dns.TypeCAA
	r, err := dnsClient.exchangeOne(ctx, hostname, dnsType)
	if err != nil {
		return nil, &DNSError{dnsType, hostname, err, -1}
	}

	if r.Rcode == dns.RcodeServerFailure {
		return nil, &DNSError{dnsType, hostname, nil, r.Rcode}
	}

	var CAAs []*dns.CAA
	for _, answer := range r.Answer {
		if caaR, ok := answer.(*dns.CAA); ok {
			CAAs = append(CAAs, caaR)
		}
	}
	return CAAs, nil
}

// LookupMX sends a DNS query to find a MX record associated hostname and returns the
// record target.
func (dnsClient *DNSClientImpl) LookupMX(ctx context.Context, hostname string) ([]string, error) {
	dnsType := dns.TypeMX
	r, err := dnsClient.exchangeOne(ctx, hostname, dnsType)
	if err != nil {
		return nil, &DNSError{dnsType, hostname, err, -1}
	}
	if r.Rcode != dns.RcodeSuccess {
		return nil, &DNSError{dnsType, hostname, nil, r.Rcode}
	}

	var results []string
	for _, answer := range r.Answer {
		if mx, ok := answer.(*dns.MX); ok {
			results = append(results, mx.Mx)
		}
	}

	return results, nil
}
