package main

import (
	"log"
	"net/http"
	"os"

	"dokinar.ik/proxy-thing/internal"
	"dokinar.ik/proxy-thing/internal/xray"
)

func main() {
	engine, err := xray.NewXrayEngine()
	if err != nil {
		log.Fatalf("xray engine init: %v", err)
	}
	defer engine.Close()

	proxies, _ := os.ReadFile("/etc/proxy-thing/proxies.txt")

	engine.AddOutbounds(string(proxies))

	proxy := internal.NewProxy(engine)
	proxy.OnFinish = func(failed []string, gtag string, res *http.Response) {
		successTag := gtag

		if successTag == "" {
			successTag = "NONE"
		}

		log.Printf("finished request for %s %s %d, fails: %+v\n", successTag, res.Request.URL.Host, res.StatusCode, failed)

		for _, tag := range failed {

			proxy.Engine.BanHostFor(tag, res.Request.URL.Host)
		}
	}

	addr := ":1337"
	log.Printf("Listening on %s", addr)
	log.Fatal(http.ListenAndServe(addr, proxy))
}
