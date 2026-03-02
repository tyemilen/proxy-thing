package xray

import (
	"context"
	"fmt"
	"log"
	"slices"
	"sort"
	"strings"
	"sync"

	"net/http"
	"time"

	"dokinar.ik/proxy-thing/internal/xlib"
	"github.com/go-co-op/gocron/v2"
	"github.com/xtls/xray-core/app/dispatcher"
	xlog "github.com/xtls/xray-core/app/log"
	"github.com/xtls/xray-core/app/policy"
	"github.com/xtls/xray-core/app/proxyman"
	clog "github.com/xtls/xray-core/common/log"
	"github.com/xtls/xray-core/common/net"
	"github.com/xtls/xray-core/common/serial"
	"github.com/xtls/xray-core/common/session"
	"github.com/xtls/xray-core/core"
)

type Host struct {
	Address  string
	BannedAt int64
}

type OutboundInfo struct {
	Tag      string
	BadHosts []Host
	Ping     time.Duration
}

type XrayLimits struct {
	HostBanTime    time.Duration
	GCIntervalTime time.Duration
}

type XrayEngine struct {
	Instance *core.Instance

	Limits    XrayLimits
	Scheduler gocron.Scheduler

	Outbounds []OutboundInfo
}

const PING_MAX_TIME = 1 * time.Hour

func NewXrayEngine(limits XrayLimits) (*XrayEngine, error) {
	cfg := &core.Config{
		App: []*serial.TypedMessage{
			serial.ToTypedMessage(&proxyman.OutboundConfig{}),
			serial.ToTypedMessage(&dispatcher.Config{}),
			serial.ToTypedMessage(&policy.Config{}),
			serial.ToTypedMessage(&xlog.Config{
				ErrorLogLevel: clog.Severity_Unknown,
				ErrorLogType:  xlog.LogType_Console,
			}),
		},
		Inbound:  []*core.InboundHandlerConfig{},
		Outbound: []*core.OutboundHandlerConfig{},
	}

	inst, err := core.New(cfg)

	if err != nil {
		return nil, err
	}

	scheduler, err := gocron.NewScheduler()

	if err != nil {
		return nil, err
	}

	engine := &XrayEngine{
		Instance:  inst,
		Scheduler: scheduler,
		Limits:    limits,
	}

	_, err = scheduler.NewJob(
		gocron.DurationJob(time.Duration(engine.Limits.GCIntervalTime)),
		gocron.NewTask(engine.GarbageWorker),
	)

	if err != nil {
		return nil, err
	}

	return engine, nil
}

func (e *XrayEngine) Start() error {
	if err := e.Instance.Start(); err != nil {
		return err
	}

	e.Scheduler.Start()

	return nil
}

func BuildDestination(network, addr string) (net.Destination, error) {
	dest, err := net.ParseDestination(network + ":" + addr)
	if err != nil {
		return net.Destination{}, err
	}
	return dest, nil
}

func (e *XrayEngine) Close() {
	log.Println("[xray] bye-bye")
	e.Instance.Close()
	e.Scheduler.Shutdown()
}

func (e *XrayEngine) GarbageWorker() {
	// todo detect dead outbounds
	// outboundManager := e.Instance.GetFeature(outbound.ManagerType()).(outbound.Manager)
	// outboundManager.RemoveHandler(context.Background(), )

	for i, outbound := range e.Outbounds {
		for j, host := range e.Outbounds[i].BadHosts {
			if time.Since(time.Unix(host.BannedAt, 0)) > time.Duration(e.Limits.HostBanTime) {
				log.Printf("GarbageWorker: Unbanning %s for %s\n", host.Address, outbound.Tag)

				e.Outbounds[i].BadHosts = slices.Delete(e.Outbounds[i].BadHosts, j, j+1)
			}
		}
	}
}

func (e *XrayEngine) GetClient(tag string) *http.Client {
	return &http.Client{
		Timeout: 15 * time.Second,
		Transport: &http.Transport{
			DisableKeepAlives: true,
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

func (e *XrayEngine) Ping(tag string) time.Duration {
	now := time.Now()

	client := e.GetClient(tag)
	resp, err := client.Head("http://google.com")

	if err != nil {
		return PING_MAX_TIME
	}
	defer resp.Body.Close()

	return time.Since(now)
}

func (e *XrayEngine) AddOutbounds(links string) {
	linkArr := strings.Split(links, "\n")

	outbounds, err := xlib.ConvertShareLinksToXrayJson(links)
	if err != nil {
		log.Fatalf("[WARN] failed to parse share links: %v", err)
	}

	loaded := 0
	var wg sync.WaitGroup
	for i, cfg := range outbounds.OutboundConfigs {
		cfg.SendThrough = nil // fixes infra/conf: unable to send through error somehow
		outbound, err := cfg.Build()

		if err != nil {
			log.Printf("[WARN] Could not build %s: %+v\n", linkArr[i], err)
			continue
		}

		outboundInfo := OutboundInfo{
			Tag:      fmt.Sprintf("proxy-%d", len(e.Outbounds)),
			BadHosts: []Host{},
		}
		outbound.Tag = outboundInfo.Tag
		e.Outbounds = append(e.Outbounds, outboundInfo)

		err = core.AddOutboundHandler(e.Instance, outbound)

		if err != nil {
			log.Printf("[WARN] Could not add %s: %+v\n", linkArr[i], err)
			e.Outbounds = e.Outbounds[:len(e.Outbounds)-1]
			continue
		}

		currentIndex := len(e.Outbounds) - 1
		currentTag := outbound.Tag

		wg.Go(func() {
			duration := e.Ping(currentTag)

			e.Outbounds[currentIndex].Ping = duration

			log.Printf("[%s] Ping %v\n", currentTag, duration.Seconds())
		})

		loaded += 1
	}
	log.Println("Waiting for ping results")
	wg.Wait()
	sort.Slice(e.Outbounds, func(i, j int) bool {
		return e.Outbounds[i].Ping < e.Outbounds[j].Ping
	})
	for i := 0; i < 5 && i < len(e.Outbounds); i++ {
		log.Printf("Top %d: %s - %v", i+1, e.Outbounds[i].Tag, e.Outbounds[i].Ping)
	}
	log.Printf("Done loading %d out of %d proxies\n", loaded, len(outbounds.OutboundConfigs))
}

func (e *XrayEngine) BanHostFor(tag string, address string) {
	for i := range e.Outbounds {
		if tag == e.Outbounds[i].Tag {
			e.Outbounds[i].BadHosts = append(e.Outbounds[i].BadHosts, Host{
				Address:  address,
				BannedAt: time.Now().Unix(),
			})
			continue
		}
	}
}

func (e *XrayEngine) PickOutboundTag(address string, oldTags []string) string {
	host := strings.Split(address, ":")[0]

	if len(e.Outbounds) > 0 {
		for i := range e.Outbounds {
			idx := slices.IndexFunc(e.Outbounds[i].BadHosts, func(h Host) bool {
				return h.Address == host
			})

			if idx == -1 && !slices.Contains(oldTags, e.Outbounds[i].Tag) {
				return e.Outbounds[i].Tag
			}
		}
	}

	return "proxy-1"
}
