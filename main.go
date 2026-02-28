package main

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"log"
	"net/http"
	"net/url"
	"os"
	"regexp"
	"strings"

	"github.com/elazarl/goproxy"
	"github.com/icza/backscanner"
	"github.com/xtls/xray-core/app/proxyman/command"
	"github.com/xtls/xray-core/common/net"
	"github.com/xtls/xray-core/common/protocol"
	"github.com/xtls/xray-core/common/serial"
	"github.com/xtls/xray-core/common/uuid"
	"github.com/xtls/xray-core/core"
	"github.com/xtls/xray-core/proxy/vless"
	"github.com/xtls/xray-core/proxy/vless/outbound"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
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
	proxy.Verbose = true

	proxy.Tr = &http.Transport{
		Proxy:             http.ProxyURL(upstream),
		TLSClientConfig:   &tls.Config{InsecureSkipVerify: true},
		DisableKeepAlives: true,
	}

	proxy.ConnectDial = proxy.NewConnectDialToProxy(xrayURL)

	proxy.OnRequest().HandleConnect(goproxy.AlwaysMitm)

	xrayLog, err := os.Open("/var/log/xray/access.log")

	if err != nil {
		log.Fatal(err)
	}

	defer xrayLog.Close()

	proxy.OnRequest().DoFunc(func(req *http.Request, ctx *goproxy.ProxyCtx) (*http.Request, *http.Response) {
		q := req.URL.Query()
		q.Set("ysclid", fmt.Sprintf("%s-%s", uuid.New(), uuid.New()))
		req.URL.RawQuery = q.Encode()

		return req, nil
	})

	MAX_RETRIES := 5
	proxy.OnResponse().DoFunc(func(resp *http.Response, ctx *goproxy.ProxyCtx) *http.Response {
		if resp == nil {
			return resp
		}

		attempt := 1
		client := &http.Client{Transport: proxy.Tr}

		for resp.StatusCode >= 400 && attempt <= MAX_RETRIES {
			newReq := ctx.Req.Clone(ctx.Req.Context())
			newReq.Header.Set("Connection", "close")
			newResp, err := client.Do(newReq)
			if err != nil {
				break
			}
			tag, err := findProxy(xrayLog, ctx.Req.URL.String())

			if err != nil {
				break
			}

			log.Printf("[%s] %s -> %d retry %d", tag, ctx.Req.URL.String(), resp.StatusCode, attempt)

			resp = newResp
			attempt++
		}

		return resp
	})

	addr := ":1337"
	log.Printf("%s", addr)
	log.Fatal(http.ListenAndServe(addr, proxy))
}

func findProxy(xrayLog *os.File, urlStr string) (string, error) {
	xStat, err := xrayLog.Stat()

	if err != nil {
		return "", err
	}

	reTag := regexp.MustCompile(`\[http-in >>|\-> (.*)\]`)
	xSize := xStat.Size()
	scanner := backscanner.New(xrayLog, int(xSize))
	for {
		line, _, err := scanner.LineBytes()
		if err != nil {
			return "", err
		}
		strLine := string(line)

		if strings.Contains(strLine, urlStr) {
			tagMatch := reTag.FindStringSubmatch(strLine)

			if len(tagMatch) > 1 {
				return tagMatch[1], nil
			}
		}
	}
}

func addNew() error {
	conn, err := grpc.NewClient("xray:8080", grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		log.Fatalf("Failed to connect: %v", err)
		return err
	}
	defer conn.Close()

	client := command.NewHandlerServiceClient(conn)

	out := &outbound.Config{
		Vnext: &protocol.ServerEndpoint{
			Address: &net.IPOrDomain{
				Address: &net.IPOrDomain_Domain{
					Domain: "domain.com",
				},
			},
			Port: 443,
			User: &protocol.User{
				Account: serial.ToTypedMessage(&vless.Account{
					Id:         "random-uuid",
					Flow:       "xtls-rprx-vision",
					Encryption: "none",
				}),
			},
		},
	}
	request := &command.AddOutboundRequest{
		Outbound: &core.OutboundHandlerConfig{
			Tag:           "test-vless",
			ProxySettings: serial.ToTypedMessage(out),
		},
	}

	r, err := client.AddOutbound(context.Background(), request)

	if err != nil {
		log.Printf("ERROR Sent: %+v\n", err)
		return err
	}
	log.Printf("Sent: %+v\n", r)

	return nil
}
