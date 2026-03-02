package internal

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"strings"
	"time"

	"dokinar.ik/proxy-thing/internal/xray"
	"github.com/elazarl/goproxy"
	"github.com/xtls/xray-core/common/session"
	"github.com/xtls/xray-core/core"
)

const MAX_TIMEOUT_RETRIES = 5
const MAX_RESPONSE_RETRIES = 2
const MAX_REDIRECTS = 3

type Proxy struct {
	*goproxy.ProxyHttpServer

	Engine *xray.XrayEngine

	OnFinish func(failedTags []string, tag string, resp *http.Response)
}

func NewProxy(engine *xray.XrayEngine) *Proxy {
	initCerts()

	gproxy := goproxy.NewProxyHttpServer()
	gproxy.Verbose = false

	proxy := &Proxy{
		ProxyHttpServer: gproxy,
		Engine:          engine,
	}

	gproxy.OnRequest().HandleConnect(goproxy.FuncHttpsHandler(proxy.handleConnect))
	gproxy.OnRequest().DoFunc(proxy.OnRequestFunc)
	gproxy.OnResponse().DoFunc(proxy.OnResponseFunc)

	return proxy
}

func (proxy *Proxy) handleConnect(host string, ctx *goproxy.ProxyCtx) (*goproxy.ConnectAction, string) {
	if strings.HasSuffix(host, ":443") {
		return goproxy.MitmConnect, host
	}
	return goproxy.OkConnect, host
}

func (proxy *Proxy) OnRequestFunc(req *http.Request, ctx *goproxy.ProxyCtx) (*http.Request, *http.Response) {
	if req.Body != nil && req.Body != http.NoBody {
		bodyBytes, _ := io.ReadAll(req.Body)
		req.Body.Close()
		req.GetBody = func() (io.ReadCloser, error) {
			return io.NopCloser(bytes.NewReader(bodyBytes)), nil
		}
		req.Body, _ = req.GetBody()
	}

	ctx.RoundTripper = goproxy.RoundTripperFunc(func(originalReq *http.Request, ctx *goproxy.ProxyCtx) (*http.Response, error) {
		tr := &http.Transport{
			TLSHandshakeTimeout:   5 * time.Second,
			ResponseHeaderTimeout: 5 * time.Second,
			IdleConnTimeout:       5 * time.Second,
			TLSClientConfig:       &tls.Config{InsecureSkipVerify: true},
			DisableKeepAlives:     true,
			DialContext: func(dialCtx context.Context, network, addr string) (net.Conn, error) {
				dest, err := xray.BuildDestination(network, addr)
				if err != nil {
					return nil, err
				}

				var tag string
				tags, _ := ctx.UserData.([]string)

				tag = proxy.Engine.PickOutboundTag(addr, tags)

				ctx.UserData = append(tags, tag)

				dialCtx = session.SetForcedOutboundTagToContext(dialCtx, tag)
				return core.Dial(dialCtx, proxy.Engine.Instance, dest)
			},
		}

		currentReq := originalReq
		for redirects := 0; redirects <= MAX_REDIRECTS; redirects++ {
			var resp *http.Response
			var err error

			for tries := 0; tries <= MAX_TIMEOUT_RETRIES; tries++ {
				attemptReq := currentReq.Clone(currentReq.Context())
				if attemptReq.GetBody != nil {
					attemptReq.Body, _ = attemptReq.GetBody()
				}

				resp, err = tr.RoundTrip(attemptReq)
				if err == nil {
					break
				}
				if netErr, ok := err.(net.Error); ok && netErr.Timeout() && tries < MAX_TIMEOUT_RETRIES {
					continue
				}
				return nil, err
			}

			if resp.StatusCode >= 300 && resp.StatusCode <= 308 && resp.StatusCode != 304 {
				loc, err := resp.Location()
				if err != nil {
					return resp, nil
				}
				resp.Body.Close()
				nextReq := currentReq.Clone(currentReq.Context())
				nextReq.URL = loc
				nextReq.Host = loc.Host
				if resp.StatusCode <= 303 {
					nextReq.Method = http.MethodGet
					nextReq.Body = nil
					nextReq.GetBody = nil
					nextReq.Header.Del("Content-Type")
					nextReq.Header.Del("Content-Length")
				}
				currentReq = nextReq
				continue
			}

			return resp, nil
		}
		return nil, fmt.Errorf("too many redirects")
	})

	return req, nil
}

func (proxy *Proxy) OnResponseFunc(resp *http.Response, ctx *goproxy.ProxyCtx) *http.Response {
	if resp == nil {
		return nil
	}

	attempt := 1
	for resp.StatusCode == 403 && attempt <= MAX_RESPONSE_RETRIES {
		resp.Body.Close()
		newReq := ctx.Req.Clone(ctx.Req.Context())
		if newReq.GetBody != nil {
			newReq.Body, _ = newReq.GetBody()
		}

		newResp, err := ctx.RoundTrip(newReq)

		allTags, _ := ctx.UserData.([]string)
		log.Printf("Retrying [%s] -> %s\n", allTags[len(allTags)-1], newReq.Host)

		if err != nil {
			log.Printf("Tag failed due to network error: %v", err)
			break
		}

		resp = newResp
		attempt++
	}

	allTags, _ := ctx.UserData.([]string)

	var successTag string
	var failedTags []string

	if len(allTags) > 0 {
		if resp.StatusCode < 400 {
			successTag = allTags[len(allTags)-1]
			failedTags = allTags[:len(allTags)-1]
		} else {
			failedTags = allTags
		}
	}

	if proxy.OnFinish != nil {
		proxy.OnFinish(failedTags, successTag, resp)
	}

	return resp
}

func initCerts() {
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
}
