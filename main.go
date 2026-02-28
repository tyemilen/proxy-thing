package main

import (
	"bufio"
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"io"
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
	proxy.Verbose = false

	proxy.Tr = &http.Transport{
		Proxy:           http.ProxyURL(upstream),
		TLSClientConfig: &tls.Config{InsecureSkipVerify: true},
	}

	proxy.ConnectDial = proxy.NewConnectDialToProxy(xrayURL)

	proxy.OnRequest().HandleConnect(goproxy.AlwaysMitm)

	xrayLog, err := os.Open("/var/log/xray/error.log")

	if err != nil {
		log.Fatal(err)
	}

	defer xrayLog.Close()

	proxy.OnResponse().DoFunc(func(resp *http.Response, ctx *goproxy.ProxyCtx) *http.Response {
		if resp != nil {
			// addNew()
			log.Printf("[%s] %s -> %d %s", ctx.Req.Method, ctx.Req.URL.Host, resp.StatusCode, resp.Status)

			if resp.StatusCode >= 400 {
				urlStr := ctx.Req.URL.String()
				tag, err := findProxy(xrayLog, ctx.Req.URL.String())

				if err != nil {
					log.Printf("err %s -> %s", urlStr, err)
				} else {
					log.Printf("%s -> %s", urlStr, tag)
				}
			}
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

	var id string
	var urlPos int

	reId := regexp.MustCompile(`\[Info\]\s+\[([^\]]+)\]`)
	xSize := xStat.Size()
	scanner := backscanner.New(xrayLog, int(xSize))
	for {
		line, pos, err := scanner.LineBytes()
		if err != nil {
			return "", err
		}
		strLine := string(line)
		if strings.Contains(strLine, urlStr) {
			idMatch := reId.FindStringSubmatch(strLine)

			if len(idMatch) > 1 {
				id = idMatch[1]
				urlPos = pos
				break
			}
		}
	}
	if len(id) <= 0 {
		return "", errors.New("id is empty")
	}

	pattern := fmt.Sprintf(`\[Info\]\s+\[%s\].*?detour\s+\[([^\]]+)\]`, regexp.QuoteMeta(id))
	reProxy, err := regexp.Compile(pattern)

	if err != nil {
		return "", errors.New("bad regexp")
	}

	section := io.NewSectionReader(xrayLog, int64(urlPos), xSize-int64(urlPos))
	fScanner := bufio.NewScanner(section)
	linesRead := 0
	for fScanner.Scan() {
		if linesRead >= 500 {
			return "", errors.New("cant find proxy tag")
		}

		line := fScanner.Text()

		if strings.Contains(line, "["+id+"]") && strings.Contains(line, "taking detour") {
			proxyMatch := reProxy.FindStringSubmatch(line)
			if len(proxyMatch) > 1 {
				proxyTag := proxyMatch[1]

				return proxyTag, nil
			}
		}

		linesRead++
	}

	return "", errors.New("=(")
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
