package main

import (
	"errors"
	"flag"
	"fmt"
	"math/rand"
	"os"
	"os/signal"
	"runtime"
	"runtime/pprof"
	"time"

	"doppler/config"
	"github.com/cloudfoundry/gosteno"
	"github.com/cloudfoundry/gunk/workpool"
	"github.com/cloudfoundry/loggregatorlib/cfcomponent"
	"github.com/cloudfoundry/loggregatorlib/cfcomponent/registrars/collectorregistrar"
	"github.com/cloudfoundry/storeadapter"
	"github.com/cloudfoundry/storeadapter/etcdstoreadapter"
	"github.com/cloudfoundry/yagnats"
	"github.com/cloudfoundry/yagnats/fakeyagnats"
	"github.com/pivotal-golang/localip"
)

var (
	logFilePath = flag.String("logFile", "", "The agent log file, defaults to STDOUT")
	logLevel    = flag.Bool("debug", false, "Debug logging")
	configFile  = flag.String("config", "config/doppler.json", "Location of the doppler config json file")
	cpuprofile  = flag.String("cpuprofile", "", "write cpu profile to file")
	memprofile  = flag.String("memprofile", "", "write memory profile to this file")
)

type DopplerServerHealthMonitor struct {
}

func (hm DopplerServerHealthMonitor) Ok() bool {
	return true
}

var StoreAdapterProvider = func(urls []string, concurrentRequests int) storeadapter.StoreAdapter {
	workPool := workpool.NewWorkPool(concurrentRequests)

	return etcdstoreadapter.NewETCDStoreAdapter(urls, workPool)
}

func main() {
	seed := time.Now().UnixNano()
	rand.Seed(seed)

	flag.Parse()

	runtime.GOMAXPROCS(runtime.NumCPU())

	localIp, err := localip.LocalIP()
	if err != nil {
		panic(errors.New("Unable to resolve own IP address: " + err.Error()))
	}

	if *cpuprofile != "" {
		f, err := os.Create(*cpuprofile)
		if err != nil {
			panic(err)
		}
		pprof.StartCPUProfile(f)
		defer func() {
			pprof.StopCPUProfile()
			f.Close()
		}()
	}

	if *memprofile != "" {
		f, err := os.Create(*memprofile)
		if err != nil {
			panic(err)
		}
		go func() {
			defer f.Close()
			ticker := time.NewTicker(time.Second * 1)
			defer ticker.Stop()
			for {
				<-ticker.C
				pprof.WriteHeapProfile(f)
			}
		}()
	}

	conf, logger := ParseConfig(logLevel, configFile, logFilePath)

	if len(conf.NatsHosts) == 0 {
		logger.Warn("Startup: Did not receive a NATS host - not going to regsiter component")
		cfcomponent.DefaultYagnatsClientProvider = func(logger *gosteno.Logger, c *cfcomponent.Config) (yagnats.NATSConn, error) {
			return fakeyagnats.Connect(), nil
		}
	}

	err = conf.Validate(logger)
	if err != nil {
		panic(err)
	}

	doppler := New(localIp, conf, logger, "doppler")

	cfc, err := cfcomponent.NewComponent(
		logger,
		"DopplerServer",
		conf.Index,
		&DopplerServerHealthMonitor{},
		conf.VarzPort,
		[]string{conf.VarzUser, conf.VarzPass},
		doppler.Emitters(),
	)

	if err != nil {
		panic(err)
	}

	go collectorregistrar.NewCollectorRegistrar(cfcomponent.DefaultYagnatsClientProvider, cfc, time.Duration(conf.CollectorRegistrarIntervalMilliseconds)*time.Millisecond, &conf.Config).Run()

	go func() {
		err := cfc.StartMonitoringEndpoints()
		if err != nil {
			panic(err)
		}
	}()

	go doppler.Start()
	logger.Info("Startup: doppler server started.")

	killChan := make(chan os.Signal)
	signal.Notify(killChan, os.Kill, os.Interrupt)

	StartHeartbeats(localIp, config.HeartbeatInterval, conf, logger)

	for {
		select {
		case <-cfcomponent.RegisterGoRoutineDumpSignalChannel():
			cfcomponent.DumpGoRoutine()
		case <-killChan:
			logger.Info("Shutting down")
			doppler.Stop()
			return
		}
	}
}

func ParseConfig(logLevel *bool, configFile, logFilePath *string) (*config.Config, *gosteno.Logger) {
	config := &config.Config{}
	err := cfcomponent.ReadConfigInto(config, *configFile)
	if err != nil {
		panic(err)
	}

	logger := cfcomponent.NewLogger(*logLevel, *logFilePath, "doppler", config.Config)
	logger.Info("Startup: Setting up the doppler server")

	return config, logger
}

func StartHeartbeats(localIp string, ttl time.Duration, config *config.Config, logger *gosteno.Logger) (stopChan chan (chan bool)) {
	if len(config.EtcdUrls) == 0 {
		return
	}

	adapter := StoreAdapterProvider(config.EtcdUrls, config.EtcdMaxConcurrentRequests)
	adapter.Connect()

	logger.Debugf("Starting Health Status Updates to Store: /healthstatus/doppler/%s/%s/%d", config.Zone, config.JobName, config.Index)
	status, stopChan, err := adapter.MaintainNode(storeadapter.StoreNode{
		Key:   fmt.Sprintf("/healthstatus/doppler/%s/%s/%d", config.Zone, config.JobName, config.Index),
		Value: []byte(localIp),
		TTL:   uint64(ttl.Seconds()),
	})

	if err != nil {
		panic(err)
	}

	go func() {
		for stat := range status {
			logger.Debugf("Health updates channel pushed %v at time %v", stat, time.Now())
		}
	}()

	return stopChan
}
