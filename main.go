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
	"sort"
	"strconv"
	"strings"
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

// loadFileRules reads rules from a JSON config file. If the file is missing
// and it was not explicitly requested (explicit == false), it is treated as
// "no file rules" rather than an error, so the proxy can run from environment
// variables alone.
func loadFileRules(path string, explicit bool) ([]Rule, error) {
	if path == "" {
		return nil, nil
	}
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) && !explicit {
			return nil, nil
		}
		return nil, err
	}
	var c Config
	if err := json.Unmarshal(data, &c); err != nil {
		return nil, fmt.Errorf("parse %s: %w", path, err)
	}
	return c.Rules, nil
}

// envRules builds rules from IMGSTEAL_RULE_MATCH[_XXX] / IMGSTEAL_RULE_REFERER[_XXX]
// environment variable pairs. The optional _XXX suffix pairs a match with its
// referer; rules are ordered by that suffix (the unsuffixed one first).
func envRules() ([]Rule, error) {
	matches := map[string]string{}
	referers := map[string]string{}
	for _, kv := range os.Environ() {
		k, v, ok := strings.Cut(kv, "=")
		if !ok {
			continue
		}
		switch {
		case k == "IMGSTEAL_RULE_MATCH":
			matches[""] = v
		case strings.HasPrefix(k, "IMGSTEAL_RULE_MATCH_"):
			matches[strings.TrimPrefix(k, "IMGSTEAL_RULE_MATCH_")] = v
		case k == "IMGSTEAL_RULE_REFERER":
			referers[""] = v
		case strings.HasPrefix(k, "IMGSTEAL_RULE_REFERER_"):
			referers[strings.TrimPrefix(k, "IMGSTEAL_RULE_REFERER_")] = v
		}
	}

	suffixes := make([]string, 0, len(matches))
	for s := range matches {
		suffixes = append(suffixes, s)
	}
	sort.Strings(suffixes)

	rules := make([]Rule, 0, len(suffixes))
	for _, s := range suffixes {
		ref, ok := referers[s]
		if !ok {
			return nil, fmt.Errorf("IMGSTEAL_RULE_MATCH%s has no matching IMGSTEAL_RULE_REFERER%s", suffixTag(s), suffixTag(s))
		}
		rules = append(rules, Rule{Match: matches[s], Referer: ref})
	}
	return rules, nil
}

func suffixTag(s string) string {
	if s == "" {
		return ""
	}
	return "_" + s
}

// compileRules compiles each rule's Match regex, preserving order.
func compileRules(specs []Rule) ([]compiledRule, error) {
	rules := make([]compiledRule, 0, len(specs))
	for i, r := range specs {
		re, err := regexp.Compile(r.Match)
		if err != nil {
			return nil, fmt.Errorf("rule %d (%q): %w", i, r.Match, err)
		}
		rules = append(rules, compiledRule{re: re, referer: r.Referer})
	}
	return rules, nil
}

func main() {
	listen := flag.String("listen", ":8080", "address to listen on (env IMGSTEAL_LISTEN)")
	rulesPath := flag.String("rules", "rules.json", "path to JSON config with domain->referer rules; rules may also be set via IMGSTEAL_RULE_MATCH[_XXX] / IMGSTEAL_RULE_REFERER[_XXX] env pairs")
	timeout := flag.Duration("timeout", 30*time.Second, "upstream request timeout (env IMGSTEAL_TIMEOUT)")
	verbose := flag.Bool("v", false, "log each request (env IMGSTEAL_VERBOSE)")
	flag.Parse()

	// Environment variables fill in values the user did not set explicitly on
	// the command line; an explicit flag always wins.
	set := map[string]bool{}
	flag.Visit(func(f *flag.Flag) { set[f.Name] = true })

	if !set["listen"] {
		if v := os.Getenv("IMGSTEAL_LISTEN"); v != "" {
			*listen = v
		}
	}
	if !set["timeout"] {
		if v := os.Getenv("IMGSTEAL_TIMEOUT"); v != "" {
			d, err := time.ParseDuration(v)
			if err != nil {
				log.Fatalf("IMGSTEAL_TIMEOUT: %v", err)
			}
			*timeout = d
		}
	}
	if !set["v"] {
		if v := os.Getenv("IMGSTEAL_VERBOSE"); v != "" {
			b, err := strconv.ParseBool(v)
			if err != nil {
				log.Fatalf("IMGSTEAL_VERBOSE: %v", err)
			}
			*verbose = b
		}
	}

	// Rules come from the JSON file (if present) followed by any defined via
	// environment variables.
	specs, err := loadFileRules(*rulesPath, set["rules"])
	if err != nil {
		log.Fatalf("rules: %v", err)
	}
	envSpecs, err := envRules()
	if err != nil {
		log.Fatalf("rules: %v", err)
	}
	specs = append(specs, envSpecs...)

	rules, err := compileRules(specs)
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
	if *verbose {
		for i, r := range rules {
			log.Printf("  rule %d: match=%q referer=%s", i, r.re.String(), r.referer)
		}
	}
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
