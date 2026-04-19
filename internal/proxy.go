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
	"slices"
	"strings"
	"time"

	"dokinar.ik/proxy-thing/internal/xlib"
	"dokinar.ik/proxy-thing/internal/xray"
	"github.com/elazarl/goproxy"
	"github.com/xtls/xray-core/common/session"
	"github.com/xtls/xray-core/core"
)

type ProxyConfig struct {
	MaxTimeoutRetries  int
	MaxResponseRetries int
	MaxRedirects       int
	RetryOn            []int
}

type Proxy struct {
	*goproxy.ProxyHttpServer

	Engine  *xray.XrayEngine
	Config  ProxyConfig
	Clients map[string]string // name : last-used-proxy

	OnFinish func(failedTags []string, tag string, resp *http.Response)
}

type ContextUserData struct {
	Tags   []string
	Client string
}

func NewProxy(engine *xray.XrayEngine, config ProxyConfig) *Proxy {
	gproxy := goproxy.NewProxyHttpServer()
	gproxy.Verbose = false

	gproxy.Tr = &http.Transport{
		TLSHandshakeTimeout:   35 * time.Second,
		ResponseHeaderTimeout: 35 * time.Second,
		IdleConnTimeout:       35 * time.Second,
		DisableKeepAlives:     false,
		TLSClientConfig:       &tls.Config{InsecureSkipVerify: true},
		DialContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
			dest, err := xray.BuildDestination(network, addr)
			if err != nil {
				return nil, err
			}

			return core.Dial(ctx, engine.Instance, dest)
		},
	}

	proxy := &Proxy{
		ProxyHttpServer: gproxy,
		Engine:          engine,
		Clients:         map[string]string{},
		Config:          config,
	}

	gproxy.OnRequest().HandleConnect(goproxy.FuncHttpsHandler(proxy.handleConnect))
	gproxy.OnRequest().DoFunc(proxy.OnRequestFunc)
	gproxy.OnResponse().DoFunc(proxy.OnResponseFunc)

	return proxy
}

func (proxy *Proxy) handleConnect(host string, ctx *goproxy.ProxyCtx) (*goproxy.ConnectAction, string) {
	userData := &ContextUserData{
		Tags: []string{},
	}

	authHeader := ctx.Req.Header.Get("Proxy-Authorization")

	if authHeader != "" {
		rawAuth, err := xlib.DecodeBase64Text(ctx.Req.Header.Get("Proxy-Authorization")[6:])
		if err != nil {
			log.Println(err)
		}

		auth := strings.Split(rawAuth, ":")
		log.Println("user: ", auth)
		userData.Client = auth[0]
	}

	ctx.UserData = userData

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
			TLSHandshakeTimeout:   25 * time.Second,
			ResponseHeaderTimeout: 25 * time.Second,
			IdleConnTimeout:       25 * time.Second,
			TLSClientConfig:       &tls.Config{InsecureSkipVerify: true},
			DisableKeepAlives:     true,
			DialContext: func(dialCtx context.Context, network, addr string) (net.Conn, error) {
				dest, err := xray.BuildDestination(network, addr)
				if err != nil {
					return nil, err
				}

				data, ok := ctx.UserData.(*ContextUserData)
				if !ok {
					data = &ContextUserData{Tags: []string{}}
					ctx.UserData = data
				}

				var tag string

				if clients := strings.Split(data.Client, "~"); len(clients) > 1 && clients[1] != "" && proxy.Clients[clients[1]] != "" {
					tag = proxy.Clients[clients[1]]
					log.Println("USing bc requested ", tag)
				} else {
					tag = proxy.Engine.PickOutboundTag(addr, data.Tags)
				}

				data.Tags = append(data.Tags, tag)

				dialCtx = session.SetForcedOutboundTagToContext(dialCtx, tag)
				return core.Dial(dialCtx, proxy.Engine.Instance, dest)
			},
		}

		currentReq := originalReq
		for redirects := 0; redirects <= proxy.Config.MaxRedirects; redirects++ {
			var resp *http.Response
			var err error

			for tries := 0; tries <= proxy.Config.MaxTimeoutRetries; tries++ {
				attemptReq := currentReq.Clone(currentReq.Context())
				if attemptReq.GetBody != nil {
					attemptReq.Body, _ = attemptReq.GetBody()
				}

				resp, err = tr.RoundTrip(attemptReq)
				if err == nil {
					break
				}
				if netErr, ok := err.(net.Error); ok && netErr.Timeout() && tries < proxy.Config.MaxTimeoutRetries {
					continue
				}
				return nil, err
			}

			// todo: move to config
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
	for slices.Contains(proxy.Config.RetryOn, resp.StatusCode) && attempt <= proxy.Config.MaxResponseRetries {
		resp.Body.Close()
		newReq := ctx.Req.Clone(ctx.Req.Context())
		if newReq.GetBody != nil {
			newReq.Body, _ = newReq.GetBody()
		}

		newResp, err := ctx.RoundTrip(newReq)

		allTags := ctx.UserData.(*ContextUserData).Tags
		log.Printf("Retrying [%s] -> %s\n", allTags[len(allTags)-1], newReq.Host)

		if err != nil {
			log.Printf("Tag failed due to network error: %v", err)
			break
		}

		resp = newResp
		attempt++
	}

	data, _ := ctx.UserData.(*ContextUserData)

	var successTag string
	var failedTags []string

	if len(data.Tags) > 0 {
		if resp.StatusCode < 400 {
			successTag = data.Tags[len(data.Tags)-1]
			failedTags = data.Tags[:len(data.Tags)-1]

			if data.Client != "" && !strings.Contains(data.Client, "~") {
				proxy.Clients[data.Client] = successTag
			}
		} else {
			failedTags = data.Tags
		}
	}

	log.Println(proxy.Clients)
	if proxy.OnFinish != nil {
		proxy.OnFinish(failedTags, successTag, resp)
	}

	return resp
}

func InitCerts(crtPath string, keyPath string) {
	caCert, err := os.ReadFile(crtPath)
	if err != nil {
		log.Fatalf("ca.crt: %v", err)
	}
	caKey, err := os.ReadFile(keyPath)
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
