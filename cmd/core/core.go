package core

import (
	"bepass/cache"
	"bepass/dialer"
	"bepass/doh"
	"bepass/logger"
	"bepass/resolve"
	"bepass/server"
	"bepass/socks5"
	"fmt"
	"log"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"
)

type Config struct {
	TLSHeaderLength        int             `mapstructure:"TLSHeaderLength"`
	TLSPaddingEnabled      bool            `mapstructure:"TLSPaddingEnabled"`
	TLSPaddingSize         [2]int          `mapstructure:"TLSPaddingSize"`
	DnsCacheTTL            int             `mapstructure:"DnsCacheTTL"`
	DnsRequestTimeout      int             `mapstructure:"DnsRequestTimeout"`
	WorkerAddress          string          `mapstructure:"WorkerAddress"`
	WorkerIPPortAddress    string          `mapstructure:"WorkerIPPortAddress"`
	WorkerEnabled          bool            `mapstructure:"WorkerEnabled"`
	WorkerDNSOnly          bool            `mapstructure:"WorkerDNSOnly"`
	EnableLowLevelSockets  bool            `mapstructure:"EnableLowLevelSockets"`
	EnableDNSFragmentation bool            `mapstructure:"EnableDNSFragmentation"`
	RemoteDNSAddr          string          `mapstructure:"RemoteDNSAddr"`
	BindAddress            string          `mapstructure:"BindAddress"`
	ChunksLengthBeforeSni  [2]int          `mapstructure:"ChunksLengthBeforeSni"`
	SniChunksLength        [2]int          `mapstructure:"SniChunksLength"`
	ChunksLengthAfterSni   [2]int          `mapstructure:"ChunksLengthAfterSni"`
	DelayBetweenChunks     [2]int          `mapstructure:"DelayBetweenChunks"`
	Hosts                  []resolve.Hosts `mapstructure:"Hosts"`
	ResolveSystem          string          `mapstructure:"-"`
	DoHClient              *doh.Client     `mapstructure:"-"`
}

var s5 *socks5.Server

func RunServer(config *Config, captureCTRLC bool) error {
	appCache := cache.NewCache(time.Duration(config.DnsCacheTTL) * time.Second)

	var resolveSystem string
	var dohClient *doh.Client

	stdLogger := log.New(os.Stderr, "", log.Ldate|log.Ltime)
	appLogger := logger.NewLogger(stdLogger)

	localResolver := &resolve.LocalResolver{
		Logger: appLogger,
		Hosts:  config.Hosts,
	}

	dialer_ := &dialer.Dialer{
		Logger:                appLogger,
		EnableLowLevelSockets: config.EnableLowLevelSockets,
		TLSPaddingEnabled:     config.TLSPaddingEnabled,
		TLSPaddingSize:        config.TLSPaddingSize,
		ProxyAddress:          fmt.Sprintf("socks5://%s", config.BindAddress),
	}

	if strings.HasPrefix(config.RemoteDNSAddr, "https://") {
		resolveSystem = "doh"
		dohClient = doh.NewClient(
			doh.WithDNSFragmentation((config.WorkerEnabled && config.WorkerDNSOnly) || config.EnableDNSFragmentation),
			doh.WithDialer(dialer_),
			doh.WithLocalResolver(localResolver),
		)
	} else {
		resolveSystem = "DNSCrypt"
	}

	chunkConfig := server.ChunkConfig{
		BeforeSniLength: config.SniChunksLength,
		AfterSniLength:  config.ChunksLengthAfterSni,
		Delay:           config.DelayBetweenChunks,
		TLSHeaderLength: config.TLSHeaderLength,
	}

	workerConfig := server.WorkerConfig{
		WorkerAddress:       config.WorkerAddress,
		WorkerIPPortAddress: config.WorkerIPPortAddress,
		WorkerEnabled:       config.WorkerEnabled,
		WorkerDNSOnly:       config.WorkerDNSOnly,
	}

	serverHandler := &server.Server{
		RemoteDNSAddr:         config.RemoteDNSAddr,
		Cache:                 appCache,
		ResolveSystem:         resolveSystem,
		DoHClient:             dohClient,
		Logger:                appLogger,
		ChunkConfig:           chunkConfig,
		WorkerConfig:          workerConfig,
		BindAddress:           config.BindAddress,
		EnableLowLevelSockets: config.EnableLowLevelSockets,
		Dialer:                dialer_,
		LocalResolver:         localResolver,
	}

	if captureCTRLC {
		c := make(chan os.Signal)
		signal.Notify(c, os.Interrupt, syscall.SIGTERM)
		go func() {
			<-c
			_ = ShutDown()
			os.Exit(0)
		}()
	}

	s5 = socks5.NewServer(
		socks5.WithConnectHandle(serverHandler.Handle),
	)

	fmt.Println("Starting socks, http server:", config.BindAddress)
	if err := s5.ListenAndServe("tcp", config.BindAddress); err != nil {
		return err
	}

	return nil
}

func ShutDown() error {
	return s5.Shutdown()
}