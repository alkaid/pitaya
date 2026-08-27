package main

import (
	"flag"
	"fmt"
	"strings"
	"time"

	"github.com/alkaid/pitaya/v2"
	"github.com/alkaid/pitaya/v2/acceptor"
	"github.com/alkaid/pitaya/v2/acceptorwrapper"
	"github.com/alkaid/pitaya/v2/component"
	"github.com/alkaid/pitaya/v2/config"
	"github.com/alkaid/pitaya/v2/examples/demo/rate_limiting/services"
	"github.com/alkaid/pitaya/v2/metrics"
	"github.com/alkaid/viper"
)

func createAcceptor(port int, reporters []metrics.Reporter) acceptor.Acceptor {

	// 5 requests in 1 minute. Doesn't make sense, just to test
	// rate limiting
	vConfig := viper.New()
	vConfig.Set("pitaya.conn.ratelimiting.limit", 5)
	vConfig.Set("pitaya.conn.ratelimiting.interval", time.Minute)
	pConfig := config.NewConfig(vConfig)

	rateLimitConfig := config.NewPitayaConfig(pConfig).Conn.RateLimiting

	tcp := acceptor.NewTCPAcceptor(fmt.Sprintf(":%d", port))
	return acceptorwrapper.WithWrappers(
		tcp,
		acceptorwrapper.NewRateLimitingWrapper(reporters, rateLimitConfig))
}

var app pitaya.Pitaya

func runApp(lifecycle pitaya.Lifecycle) {
	defer lifecycle.Shutdown()
	lifecycle.Start()
}

func main() {
	port := flag.Int("port", 3250, "the port to listen")
	svType := "room"

	flag.Parse()

	config := config.NewDefaultPitayaConfig()
	builder := pitaya.NewDefaultBuilder(true, svType, pitaya.Cluster, map[string]string{}, *config)
	builder.AddAcceptor(createAcceptor(*port, builder.MetricsReporters))

	app = builder.Build()

	room := services.NewRoom()
	app.Register(room,
		component.WithName("room"),
		component.WithNameFunc(strings.ToLower),
	)

	runApp(app)
}
