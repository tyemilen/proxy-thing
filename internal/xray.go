package internal

import (
	"context"
	"fmt"
	"log"
	"strings"

	"net/http"
	"time"

	"dokinar.ik/proxy-thing/internal/xlib"
	"github.com/xtls/xray-core/app/dispatcher"
	"github.com/xtls/xray-core/app/policy"
	"github.com/xtls/xray-core/app/proxyman"
	"github.com/xtls/xray-core/common/net"
	"github.com/xtls/xray-core/common/serial"
	"github.com/xtls/xray-core/common/session"
	"github.com/xtls/xray-core/core"
)

type XrayEngine struct {
	Instance *core.Instance
	Tags     []string
}

func NewXrayEngine() (*XrayEngine, error) {
	cfg := &core.Config{
		App: []*serial.TypedMessage{
			serial.ToTypedMessage(&proxyman.OutboundConfig{}),
			serial.ToTypedMessage(&dispatcher.Config{}),
			serial.ToTypedMessage(&policy.Config{}),
		},
		Inbound:  []*core.InboundHandlerConfig{},
		Outbound: []*core.OutboundHandlerConfig{},
	}

	inst, err := core.New(cfg)
	if err != nil {
		return nil, err
	}

	if err := inst.Start(); err != nil {
		return nil, err
	}

	return &XrayEngine{
		Instance: inst,
	}, nil
}

func BuildDestination(network, addr string) (net.Destination, error) {
	dest, err := net.ParseDestination(network + ":" + addr)
	if err != nil {
		return net.Destination{}, err
	}
	return dest, nil
}

func (e *XrayEngine) GetClient(tag string) *http.Client {
	return &http.Client{
		Timeout: 15 * time.Second,
		Transport: &http.Transport{
			DialContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
				dest, err := BuildDestination(network, addr)
				if err != nil {
					return nil, err
				}

				ctx = session.SetForcedOutboundTagToContext(ctx, tag)

				return core.Dial(ctx, e.Instance, dest)
			},
		},
	}
}

func (e *XrayEngine) AddOutbounds(links string) {
	linkArr := strings.Split(links, "\n")

	outbounds, err := xlib.ConvertShareLinksToXrayJson(links)
	if err != nil {
		log.Fatalf("[WARN] failed to parse share links: %v", err)
	}

	for i, cfg := range outbounds.OutboundConfigs {
		cfg.SendThrough = nil // fixes infra/conf: unable to send through error somehow
		outbound, err := cfg.Build()

		if err != nil {
			log.Printf("[WARN] Could not build %s\n", linkArr[i])
			continue
		}

		outbound.Tag = fmt.Sprintf("proxy-%d", len(e.Tags))
		e.Tags = append(e.Tags, outbound.Tag)

		err = core.AddOutboundHandler(e.Instance, outbound)

		if err != nil {
			log.Printf("[WARN] Could not add %s\n", linkArr[i])
			e.Tags = e.Tags[:len(e.Tags)-1]
			continue
		}

		log.Printf("New proxy %s %s...\n", e.Tags[len(e.Tags)-1], linkArr[i])
	}
}
