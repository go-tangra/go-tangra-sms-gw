package parity

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"time"
)

var (
	// JSONCT and FormCT are the two request codecs clients use.
	JSONCT = [2]string{"Content-Type", "application/json"}
	FormCT = [2]string{"Content-Type", "application/x-www-form-urlencoded"}
	// PHPHeaders are what the PHP SDK (sdk/php, Guzzle 7) sends on every
	// request: JSON Accept and Content-Type and Guzzle's User-Agent.
	PHPHeaders = [][2]string{{"Accept", "application/json"}, {"Content-Type", "application/json"}, {"User-Agent", "GuzzleHttp/7"}}
)

// Bearer is an Authorization header carrying the variable v.
func Bearer(v string) [2]string { return [2]string{"Authorization", "Bearer {{" + v + "}}"} }

// PHP returns the PHP SDK headers plus h.
func PHP(h ...[2]string) [][2]string { return append(append([][2]string{}, PHPHeaders...), h...) }

// Post is a POST with a JSON content type unless h names one.
func Post(path, body string, h ...[2]string) Request {
	if body != "" && !hasHeader(h, "Content-Type") {
		h = append([][2]string{JSONCT}, h...)
	}
	return Request{Method: "POST", Path: path, Header: h, Body: body}
}

// Get is a GET.
func Get(path string, h ...[2]string) Request { return Request{Method: "GET", Path: path, Header: h} }

// Method is a body-less request with any method.
func Method(m, path string, h ...[2]string) Request { return Request{Method: m, Path: path, Header: h} }

func hasHeader(h [][2]string, k string) bool {
	for _, e := range h {
		if strings.EqualFold(e[0], k) {
			return true
		}
	}
	return false
}

func b64(b []byte) string { return base64.RawURLEncoding.EncodeToString(b) }

// Forge signs claims as a JWT (alg "none" leaves the signature empty).
func Forge(claims map[string]any, alg, secret string) string {
	head := b64([]byte(fmt.Sprintf(`{"alg":%q,"typ":"JWT"}`, alg)))
	body, _ := json.Marshal(claims)
	unsigned := head + "." + b64(body)
	if alg == "none" {
		return unsigned + "."
	}
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write([]byte(unsigned))
	return unsigned + "." + b64(mac.Sum(nil))
}

// Claims are the Hermes token claims.
func Claims(sub, user, authority, kind string, iat, exp time.Time) map[string]any {
	c := map[string]any{"iss": "sms-gw", "sub": sub, "username": user, "authority": authority, "iat": iat.Unix(), "nbf": iat.Unix(),
		"exp": exp.Unix(), "jti": "0123456789abcdef0123456789abcdef"}
	if kind != "" {
		c["kind"] = kind
	}
	return c
}

// Flood sends n identical requests with the given concurrency and returns
// the first 429 (or the last response) plus a status histogram note.
func Flood(s *Side, n, conc int, r Request) (Response, string) {
	var (
		mu    sync.Mutex
		first *Response
		last  Response
		hist  = map[int]int{}
		wg    sync.WaitGroup
		sem   = make(chan struct{}, conc)
	)
	for range n {
		wg.Add(1)
		sem <- struct{}{}
		go func() {
			defer wg.Done()
			defer func() { <-sem }()
			res := s.Do(r)
			mu.Lock()
			defer mu.Unlock()
			hist[res.Status]++
			last = res
			if res.Status == 429 && first == nil {
				first = &res
			}
		}()
	}
	wg.Wait()
	note := fmt.Sprintf("%s: %d requests -> %v", s.Name, n, hist)
	if first != nil {
		return *first, note
	}
	return last, note
}
