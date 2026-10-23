// Command fakebackend stands in for an sms-gw instance behind the canary
// balancer in deploy/canary/test.sh.
//
//	fakebackend serve -name v3 -addr 127.0.0.1:19901
//	    Answers every request with a JSON echo naming the instance and what it
//	    received (method, path, raw query, headers, body length, peer). GET
//	    /health answers {"status":"ok"} like both real versions.
//	fakebackend mint -secret s -user jobsbg -sub 1 -kind access
//	    Prints an HS256 JWT shaped like the services' tokens (iss sms-gw, sub,
//	    username, authority, kind, jti, iat/nbf/exp).
package main

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"time"
)

func main() {
	if len(os.Args) < 2 {
		log.Fatal("usage: fakebackend serve|mint [flags]")
	}
	switch os.Args[1] {
	case "serve":
		serve(os.Args[2:])
	case "mint":
		mint(os.Args[2:])
	default:
		log.Fatalf("unknown command %q", os.Args[1])
	}
}

func serve(args []string) {
	fs := flag.NewFlagSet("serve", flag.ExitOnError)
	name := fs.String("name", "v3", "instance name echoed in every answer")
	addr := fs.String("addr", "127.0.0.1:19901", "listen address")
	_ = fs.Parse(args)

	h := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/health" && r.Method == http.MethodGet {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte("{\"status\":\"ok\"}\n"))
			return
		}
		n, _ := io.Copy(io.Discard, r.Body)
		hdr := map[string]string{}
		for k, v := range r.Header {
			hdr[k] = fmt.Sprint(v)
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"backend":  *name,
			"method":   r.Method,
			"path":     r.URL.Path,
			"query":    r.URL.RawQuery,
			"headers":  hdr,
			"body_len": n,
			"remote":   r.RemoteAddr,
			"proto":    r.Proto,
		})
	})
	srv := &http.Server{Addr: *addr, Handler: h, ReadHeaderTimeout: 10 * time.Second}
	log.Fatal(srv.ListenAndServe())
}

func mint(args []string) {
	fs := flag.NewFlagSet("mint", flag.ExitOnError)
	secret := fs.String("secret", "test-secret", "HS256 secret")
	user := fs.String("user", "", "username claim")
	sub := fs.String("sub", "1", "subject (client id)")
	kind := fs.String("kind", "access", "access|refresh")
	_ = fs.Parse(args)

	var jti [16]byte
	_, _ = rand.Read(jti[:])
	now := time.Now().Unix()
	claims := map[string]any{
		"authority": "API_CLIENT", "kind": *kind, "username": *user,
		"iss": "sms-gw", "sub": *sub, "iat": now, "nbf": now, "exp": now + 7200,
		"jti": hex.EncodeToString(jti[:]),
	}
	if *user == "" {
		delete(claims, "username")
	}
	enc := base64.RawURLEncoding
	head := enc.EncodeToString([]byte(`{"alg":"HS256","typ":"JWT"}`))
	body, _ := json.Marshal(claims)
	signing := head + "." + enc.EncodeToString(body)
	mac := hmac.New(sha256.New, []byte(*secret))
	mac.Write([]byte(signing))
	fmt.Println(signing + "." + enc.EncodeToString(mac.Sum(nil)))
}
