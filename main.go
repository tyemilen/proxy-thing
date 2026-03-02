package main

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"io"
	"log"
	"math/rand/v2"
	"net/http"
	"os"
	"strings"
	"time"

	"dokinar.ik/proxy-thing/internal"
	"github.com/elazarl/goproxy"
	"github.com/xtls/xray-core/common/net"
	"github.com/xtls/xray-core/common/session"
	"github.com/xtls/xray-core/common/uuid"
	"github.com/xtls/xray-core/core"
	_ "github.com/xtls/xray-core/transport/internet/tcp"
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

	engine, err := internal.NewXrayEngine()
	if err != nil {
		log.Fatalf("xray engine init: %v", err)
	}
	defer engine.Instance.Close()

	proxies, _ := os.ReadFile("/etc/proxy-thing/proxies.txt")

	engine.AddOutbounds(string(proxies))

	proxy := goproxy.NewProxyHttpServer()
	proxy.Verbose = false

	proxy.Tr = &http.Transport{
		TLSHandshakeTimeout:   time.Second * 15,
		ResponseHeaderTimeout: time.Second * 15,
		DialContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
			dest, err := internal.BuildDestination(network, addr)
			if err != nil {
				return nil, err
			}

			tag := pickOutboundTag(addr, engine)
			ctx = session.SetForcedOutboundTagToContext(ctx, tag)
			conn, err := core.Dial(ctx, engine.Instance, dest)
			if err != nil {
				log.Printf("xray dial error: %v", err)
				return nil, err
			}
			return conn, nil
		},
		IdleConnTimeout: 1 * time.Second,
		TLSClientConfig: &tls.Config{InsecureSkipVerify: true},
	}
	proxy.ConnectDial = func(network, addr string) (net.Conn, error) {
		dest, err := internal.BuildDestination(network, addr)
		if err != nil {
			return nil, err
		}
		ctx := context.Background()
		tag := pickOutboundTag(addr, engine)
		ctx = session.SetForcedOutboundTagToContext(ctx, tag)

		return core.Dial(ctx, engine.Instance, dest)
	}
	proxy.OnRequest().HandleConnect(goproxy.FuncHttpsHandler(func(host string, ctx *goproxy.ProxyCtx) (*goproxy.ConnectAction, string) {
		if strings.HasSuffix(host, ":443") {
			return goproxy.MitmConnect, host
		}
		return goproxy.OkConnect, host
	}))

	proxy.OnRequest().DoFunc(func(req *http.Request, ctx *goproxy.ProxyCtx) (*http.Request, *http.Response) {
		if strings.Contains(ctx.Req.Host, "telegram") {
			return req, nil
		}

		q := req.URL.Query()
		q.Add("ysclid", fmt.Sprintf("%s-%s", uuid.New(), uuid.New()))
		req.URL.RawQuery = q.Encode()
		return req, nil
	})

	proxy.OnRequest().DoFunc(func(req *http.Request, ctx *goproxy.ProxyCtx) (*http.Request, *http.Response) {
		if req.Body != nil && req.Body != http.NoBody {
			bodyBytes, _ := io.ReadAll(req.Body)
			req.Body.Close()

			req.GetBody = func() (io.ReadCloser, error) {
				return io.NopCloser(bytes.NewReader(bodyBytes)), nil
			}
			req.Body, _ = req.GetBody()
		}
		return req, nil
	})

	const MAX_RETRIES = 5
	const MAX_REDIRECTS = 10
	const MAX_RESPONSE_RETRIES = 5

	proxy.OnRequest().DoFunc(func(req *http.Request, ctx *goproxy.ProxyCtx) (*http.Request, *http.Response) {
		if strings.Contains(req.Host, "telegram") {
			return req, nil
		}
		current := req

		for redirects := 0; redirects < MAX_REDIRECTS; redirects++ {
			var (
				resp *http.Response
				err  error
			)

			for tries := 0; tries <= MAX_RETRIES; tries++ {
				if tries > 0 && current.GetBody != nil {
					current.Body, _ = current.GetBody()
				}
				resp, err = proxy.Tr.RoundTrip(current)
				if err == nil {
					break
				}
				if netErr, ok := err.(net.Error); ok && netErr.Timeout() {
					log.Printf("Network timeout (attempt %d/%d) for %s: %v", tries+1, MAX_RETRIES, current.URL, err)
					continue
				}
				return current, goproxy.NewResponse(current, goproxy.ContentTypeText, 502, "Bad Gateway: "+err.Error())
			}

			if resp == nil {
				return current, goproxy.NewResponse(current, goproxy.ContentTypeText, 504, "Gateway Timeout: "+err.Error())
			}

			if resp.StatusCode >= 300 && resp.StatusCode <= 308 && resp.StatusCode != 304 {
				loc, err := resp.Location()
				if err != nil {
					return current, resp
				}

				current.URL = loc
				current.Host = loc.Host

				switch resp.StatusCode {
				case 301, 302, 303:
					current.Method = http.MethodGet
					current.Body, current.GetBody = nil, nil
					current.ContentLength = 0
					current.Header.Del("Content-Type")
					current.Header.Del("Content-Length")
				case 307, 308:
					if current.GetBody != nil {
						current.Body, _ = current.GetBody()
					}
				}

				resp.Body.Close()
				continue
			}

			return current, resp
		}

		return current, goproxy.NewResponse(current, goproxy.ContentTypeText, 508, "Loop Detected: Too many redirects")
	})
	proxy.OnResponse().DoFunc(func(resp *http.Response, ctx *goproxy.ProxyCtx) *http.Response {
		if resp == nil {
			return resp
		}

		resp.Close = true
		resp.Header.Set("Connection", "close")
		q := ctx.Req.URL.Query()
		q.Del("ysclid")
		ctx.Req.URL.RawQuery = q.Encode()
		attempt := 1
		for resp.StatusCode >= 400 && attempt <= MAX_RESPONSE_RETRIES {
			newReq := ctx.Req.Clone(ctx.Req.Context())

			if newReq.GetBody != nil {
				newReq.Body, _ = newReq.GetBody()
			}

			newResp, err := proxy.Tr.RoundTrip(newReq)
			if err != nil {
				log.Printf("OnResponse retry %d failed: %v", attempt, err)
				break
			}

			log.Printf("Retry %d: %s -> %d", attempt, ctx.Req.URL.String(), newResp.StatusCode)
			resp = newResp
			attempt++
		}

		return resp
	})

	addr := ":1337"
	log.Printf("Listening on %s", addr)
	log.Fatal(http.ListenAndServe(addr, proxy))
}

func pickOutboundTag(_ string, engine *internal.XrayEngine) string {
	if len(engine.Tags) > 0 {
		min := 0
		max := len(engine.Tags) - 1
		return engine.Tags[rand.IntN(max-min+1)+min]
	}
	return "proxy-1"
}
