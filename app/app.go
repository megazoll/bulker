package app

import (
	"context"
	"fmt"
	"github.com/confluentinc/confluent-kafka-go/kafka"
	"github.com/jitsucom/bulker/base/logging"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"
)

var exitChannel = make(chan os.Signal, 1)

type AppContext struct {
	config              *AppConfig
	kafkaConfig         *kafka.ConfigMap
	configurationSource ConfigurationSource
	repository          *Repository
	cron                *Cron
	producer            *Producer
	eventsLogService    EventsLogService
	topicManager        *TopicManager
	fastStore           *FastStore
	server              *http.Server
}

// TODO: graceful shutdown and cleanups. Flush producer
func Run() {
	logging.SetTextFormatter()

	signal.Notify(exitChannel, os.Interrupt, os.Kill, syscall.SIGTERM)

	appContext := InitAppContext()

	go func() {
		signal := <-exitChannel
		logging.Infof("Received signal: %s. Shutting down...", signal)
		appContext.Shutdown()
		os.Exit(0)
	}()
	logging.Info(appContext.server.ListenAndServe())
}

func Exit() {
	logging.Infof("App Triggered Exit...")
	exitChannel <- os.Interrupt
}

func InitAppContext() *AppContext {
	appContext := AppContext{}
	var err error
	appContext.config, err = InitAppConfig()
	if err != nil {
		panic(err)
	}
	appContext.kafkaConfig = appContext.config.GetKafkaConfig()

	if err != nil {
		panic(err)
	}
	appContext.configurationSource, err = InitConfigurationSource(appContext.config)
	if err != nil {
		panic(err)
	}
	appContext.repository, err = NewRepository(appContext.config, appContext.configurationSource)
	if err != nil {
		panic(err)
	}
	appContext.cron = NewCron(appContext.config)
	appContext.producer, err = NewProducer(appContext.config, appContext.kafkaConfig)
	if err != nil {
		panic(err)
	}
	appContext.producer.Start()

	appContext.eventsLogService = &DummyEventsLogService{}
	if appContext.config.EventsLogRedisURL != "" {
		appContext.eventsLogService, err = NewRedisEventsLog(appContext.config)
		if err != nil {
			panic(err)
		}
	}

	appContext.topicManager, err = NewTopicManager(&appContext)
	if err != nil {
		panic(err)
	}
	appContext.topicManager.Start()

	appContext.fastStore, err = NewFastStore(appContext.config)
	if err != nil {
		panic(err)
	}

	router := NewRouter(&appContext)
	appContext.server = &http.Server{
		Addr:              fmt.Sprintf("0.0.0.0:%d", appContext.config.HTTPPort),
		Handler:           router.GetEngine(),
		ReadTimeout:       time.Second * 60,
		ReadHeaderTimeout: time.Second * 60,
		IdleTimeout:       time.Second * 65,
	}
	return &appContext
}

func (a *AppContext) Shutdown() {
	_ = a.producer.Close()
	_ = a.topicManager.Close()
	a.cron.Close()
	_ = a.repository.Close()
	_ = a.configurationSource.Close()
	if a.config.ShutdownExtraDelay > 0 {
		logging.Infof("Waiting %d seconds before http server shutdown...", a.config.ShutdownExtraDelay)
		time.Sleep(time.Duration(a.config.ShutdownExtraDelay) * time.Second)
	}
	_ = a.server.Shutdown(context.Background())
	_ = a.eventsLogService.Close()
}
