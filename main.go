package main

import (
	"crypto/tls"
	"crypto/x509"
	"log"
	"net/http"
	"net/url"
	"os"

	"github.com/elazarl/goproxy"
)

func main() {
	caCert, err := os.ReadFile("/etc/proxy-thing/ca.crt")
	if err != nil {
		log.Fatalf("ca.crt: %v", err)
	}
	caKey, err := os.ReadFile("/etc/proxy-thing/ca.key")
	if err != nil {
		log.Fatalf("ca.key: %v", err)
	}

	goproxyCa, err := tls.X509KeyPair(caCert, caKey)
	if err != nil {
		log.Fatalf("parse CA: %v", err)
	}
	if goproxyCa.Leaf, err = x509.ParseCertificate(goproxyCa.Certificate[0]); err != nil {
		log.Fatalf("parse CA leaf: %v", err)
	}
	goproxy.GoproxyCa = goproxyCa

	xrayURL := "http://xray:1080"
	upstream, _ := url.Parse(xrayURL)

	proxy := goproxy.NewProxyHttpServer()
	proxy.Verbose = false

	proxy.Tr = &http.Transport{
		Proxy:           http.ProxyURL(upstream),
		TLSClientConfig: &tls.Config{InsecureSkipVerify: true},
	}

	proxy.ConnectDial = proxy.NewConnectDialToProxy(xrayURL)

	proxy.OnRequest().HandleConnect(goproxy.AlwaysMitm)

	proxy.OnResponse().DoFunc(func(resp *http.Response, ctx *goproxy.ProxyCtx) *http.Response {
		if resp != nil {
			log.Printf("[%s] %s -> %d %s", ctx.Req.Method, ctx.Req.URL.Host, resp.StatusCode, resp.Status)
		}
		return resp
	})

	addr := ":1337"
	log.Printf("%s", addr)
	log.Fatal(http.ListenAndServe(addr, proxy))
}
