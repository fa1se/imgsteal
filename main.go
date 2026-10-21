// Command imgsteal is a small reverse proxy that forwards an incoming request
// to a target URL, optionally overriding the Referer header based on a set of
// regex rules matched against the target's domain.
//
// The target URL is taken, url-encoded, from the "url" query parameter of the
// incoming request. Every other header on the incoming request is passed
// through to the upstream endpoint unchanged. The Referer is set only when a
// config rule matches the target host; if nothing matches, the incoming
// Referer (if any) is left untouched.
//
//	GET /?url=https%3A%2F%2Fexample.com%2Fimage.jpg
//
// Start it with:
//
//	go run . -listen :8080 -rules rules.json
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"os"
	"regexp"
	"time"
)

// hopByHop headers are connection-specific and must not be forwarded by a
// proxy. See RFC 7230 section 6.1.
var hopByHop = map[string]bool{
	"Connection":          true,
	"Proxy-Connection":    true,
	"Keep-Alive":          true,
	"Proxy-Authenticate":  true,
	"Proxy-Authorization": true,
	"Te":                  true,
	"Trailer":             true,
	"Transfer-Encoding":   true,
	"Upgrade":             true,
}

// Config is the JSON config format: an ordered list of rules. The first rule
// whose Match regex matches the target host wins.
type Config struct {
	Rules []Rule `json:"rules"`
}

// Rule maps a domain pattern to a Referer to send upstream.
type Rule struct {
	Match   string `json:"match"`   // regex, tested against the target host (no port)
	Referer string `json:"referer"` // value to set for Referer when Match hits
}

type compiledRule struct {
	re      *regexp.Regexp
	referer string
}

func loadConfig(path string) ([]compiledRule, error) {
	if path == "" {
		return nil, nil
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var c Config
	if err := json.Unmarshal(data, &c); err != nil {
		return nil, fmt.Errorf("parse %s: %w", path, err)
	}
	rules := make([]compiledRule, 0, len(c.Rules))
	for i, r := range c.Rules {
		re, err := regexp.Compile(r.Match)
		if err != nil {
			return nil, fmt.Errorf("rule %d (%q): %w", i, r.Match, err)
		}
		rules = append(rules, compiledRule{re: re, referer: r.Referer})
	}
	return rules, nil
}

func main() {
	listen := flag.String("listen", ":8080", "address to listen on")
	rulesPath := flag.String("rules", "rules.json", "path to JSON config with domain->referer rules")
	timeout := flag.Duration("timeout", 30*time.Second, "upstream request timeout")
	verbose := flag.Bool("v", false, "log each request (method, target, chosen referer, status, timing)")
	flag.Parse()

	rules, err := loadConfig(*rulesPath)
	if err != nil {
		log.Fatalf("rules: %v", err)
	}

	h := &proxy{rules: rules, verbose: *verbose}
	h.client = &http.Client{
		Timeout: *timeout,
		// Keep any rule-chosen Referer across redirects rather than letting
		// the stdlib strip it on cross-origin hops.
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			if len(via) >= 10 {
				return http.ErrUseLastResponse
			}
			if ref := h.refererFor(req.URL.Hostname()); ref != "" {
				req.Header.Set("Referer", ref)
			}
			return nil
		},
	}

	log.Printf("imgsteal listening on %s, %d referer rule(s) loaded", *listen, len(rules))
	log.Fatal(http.ListenAndServe(*listen, h))
}

// queryParam is the fixed query parameter holding the url-encoded target.
const queryParam = "url"

type proxy struct {
	client  *http.Client
	rules   []compiledRule
	verbose bool
}

// refererFor returns the Referer to set for a target host, or "" if no rule
// matches (meaning: leave the Referer untouched).
func (p *proxy) refererFor(host string) string {
	for _, r := range p.rules {
		if r.re.MatchString(host) {
			return r.referer
		}
	}
	return ""
}

func (p *proxy) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	start := time.Now()

	// The target arrives url-encoded in the "url" query parameter.
	raw := r.URL.Query().Get(queryParam)
	if raw == "" {
		p.logf("%s %s -> 400 (missing %s param) from %s", r.Method, r.URL.RequestURI(), queryParam, r.RemoteAddr)
		http.Error(w, "missing "+queryParam+" query parameter", http.StatusBadRequest)
		return
	}
	target, err := url.Parse(raw)
	if err != nil || !target.IsAbs() {
		p.logf("%s -> 400 (invalid target %q) from %s", r.Method, raw, r.RemoteAddr)
		http.Error(w, "invalid target url", http.StatusBadRequest)
		return
	}

	outReq, err := http.NewRequestWithContext(r.Context(), r.Method, target.String(), r.Body)
	if err != nil {
		http.Error(w, "bad request: "+err.Error(), http.StatusBadGateway)
		return
	}

	// Pass through every incoming header except hop-by-hop ones.
	copyHeaders(outReq.Header, r.Header)
	// Override Referer only when a rule matches the target host; otherwise the
	// incoming Referer (copied above) is left as-is.
	ref := p.refererFor(target.Hostname())
	if ref != "" {
		outReq.Header.Set("Referer", ref)
	}
	// Let the upstream host see its own Host, not the proxy's.
	outReq.Host = target.Host

	resp, err := p.client.Do(outReq)
	if err != nil {
		p.logf("%s %s -> 502 (upstream error: %v) referer=%s", r.Method, target, err, refererDesc(ref))
		http.Error(w, "upstream error: "+err.Error(), http.StatusBadGateway)
		return
	}
	defer resp.Body.Close()

	copyHeaders(w.Header(), resp.Header)
	w.WriteHeader(resp.StatusCode)
	n, _ := io.Copy(w, resp.Body)

	p.logf("%s %s -> %d %dB referer=%s %s", r.Method, target, resp.StatusCode, n, refererDesc(ref), time.Since(start).Round(time.Millisecond))
}

// logf logs only when verbose mode (-v) is enabled.
func (p *proxy) logf(format string, args ...any) {
	if p.verbose {
		log.Printf(format, args...)
	}
}

// refererDesc describes the chosen Referer for logs.
func refererDesc(ref string) string {
	if ref == "" {
		return "(passthrough)"
	}
	return ref
}

// copyHeaders copies src into dst, skipping hop-by-hop headers.
func copyHeaders(dst, src http.Header) {
	for k, vs := range src {
		if hopByHop[http.CanonicalHeaderKey(k)] {
			continue
		}
		for _, v := range vs {
			dst.Add(k, v)
		}
	}
}
