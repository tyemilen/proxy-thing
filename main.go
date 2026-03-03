package main

import (
	"encoding/json"
	"log"
	"net/http"
	"os"
	"time"

	"dokinar.ik/proxy-thing/internal"
	"dokinar.ik/proxy-thing/internal/xray"
)

type Config struct {
	Address string `json:"address"`
	Limits  struct {
		BanTime            int `json:"banTime"`
		GCInterval         int `json:"gcIntervalTime"`
		MaxTimeoutRetries  int `json:"maxTimeoutRetries"`
		MaxResponseRetries int `json:"maxResponseRetries"`
		MaxRedirects       int `json:"maxRedirects"`
	} `json:"limits"`
	Paths struct {
		Crt     string `json:"crt"`
		Key     string `json:"key"`
		Proxies string `json:"proxies"`
	} `json:"paths"`
}

func main() {
	configPath := "./config.json"

	if len(os.Args[1:]) > 0 {
		configPath = os.Args[1:][0]
	}

	rawConfig, err := os.ReadFile(configPath)

	if err != nil {
		log.Fatalln(err)
	}
	var config Config
	err = json.Unmarshal(rawConfig, &config)

	if err != nil {
		log.Fatalln(err)
	}

	log.Println(config)

	engine, err := xray.NewXrayEngine(xray.XrayLimits{
		GCIntervalTime: time.Duration(config.Limits.GCInterval) * time.Second,
		HostBanTime:    time.Duration(config.Limits.BanTime) * time.Second,
	})

	if err != nil {
		log.Fatalf("xray engine init: %v", err)
	}

	defer engine.Close()

	internal.InitCerts(config.Paths.Crt, config.Paths.Key)

	proxies, _ := os.ReadFile(config.Paths.Proxies)

	engine.AddOutbounds(string(proxies))

	proxy := internal.NewProxy(engine)

	proxy.Limits.MaxRedirects = config.Limits.MaxRedirects
	proxy.Limits.MaxResponseRetries = config.Limits.MaxResponseRetries
	proxy.Limits.MaxTimeoutRetries = config.Limits.MaxTimeoutRetries

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

	log.Printf("Listening on %s", config.Address)
	err = http.ListenAndServe(config.Address, proxy)

	if err != nil {
		engine.Close()
		log.Fatalf("HTTP server error: %v\n", err)
	}
}
