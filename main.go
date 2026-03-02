package main

import (
	"log"
	"math/rand/v2"
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
	defer engine.Instance.Close()

	proxies, _ := os.ReadFile("/etc/proxy-thing/proxies.txt")

	engine.AddOutbounds(string(proxies))

	proxy := internal.NewProxy(engine, pickOutboundTag)
	proxy.OnFinish = func(failed []string, tag string, res *http.Response) {
		log.Printf("finished request for %s %s %d, fails: %+v\n", tag, res.Request.URL.String(), res.StatusCode, failed)
	}

	addr := ":1337"
	log.Printf("Listening on %s", addr)
	log.Fatal(http.ListenAndServe(addr, proxy))
}

func pickOutboundTag(address string, engine *xray.XrayEngine) string {
	log.Println(address)
	if len(engine.Tags) > 0 {
		min := 0
		max := len(engine.Tags) - 1
		return engine.Tags[rand.IntN(max-min+1)+min]
	}
	return "proxy-1"
}
