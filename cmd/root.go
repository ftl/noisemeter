package cmd

import (
	"context"
	"fmt"
	"log"
	"math"
	"net"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"

	mqtt "github.com/eclipse/paho.mqtt.golang"
	tci "github.com/ftl/tci/client"
	dsp "github.com/mjibson/go-dsp/fft"
	"github.com/spf13/cobra"
)

var rootFlags = struct {
	tciHost      string
	trx          int
	mqttAddress  string
	mqttTopic    string
	mqttUsername string
	mqttPassword string
	filename     string
	scanInterval time.Duration
}{}

var rootCmd = &cobra.Command{
	Use:   "noisemeter",
	Short: "measure the noise on the band",
	Run:   run,
}

func Execute() {
	if err := rootCmd.Execute(); err != nil {
		fmt.Println(err)
		os.Exit(1)
	}
}

func init() {
	rootCmd.PersistentFlags().DurationVar(&rootFlags.scanInterval, "interval", time.Minute, "Scan interval")
	rootCmd.PersistentFlags().StringVar(&rootFlags.tciHost, "tci", "localhost:40001", "Connect to this TCI host")
	rootCmd.PersistentFlags().IntVar(&rootFlags.trx, "trx", 0, "Use this TRX of the TCI host")
	rootCmd.PersistentFlags().StringVar(&rootFlags.mqttAddress, "mqtt_broker", "", "Publish to this MQTT broker")
	rootCmd.PersistentFlags().StringVar(&rootFlags.mqttTopic, "mqtt_topic", "afu/noise", "Publish to this MQTT topic")
	rootCmd.PersistentFlags().StringVar(&rootFlags.mqttUsername, "mqtt_username", "", "Use this username for MQTT")
	rootCmd.PersistentFlags().StringVar(&rootFlags.mqttPassword, "mqtt_password", "", "Use this password for MQTT")
	rootCmd.PersistentFlags().StringVar(&rootFlags.filename, "filename", "noise.csv", "Use this file to log the noise")
}

func run(cmd *cobra.Command, args []string) {
	tciHost, err := parseTCPAddrArg(rootFlags.tciHost, "localhost", 40001)
	if err != nil {
		log.Fatalf("invalid tci_host: %v", err)
	}
	if tciHost.Port == 0 {
		tciHost.Port = tci.DefaultPort
	}

	var mqttClient mqtt.Client
	if rootFlags.mqttAddress != "" {
		opts := mqtt.NewClientOptions()
		opts.AddBroker(fmt.Sprintf("tcp://%s", rootFlags.mqttAddress))
		opts.SetClientID("noisemeter")
		if rootFlags.mqttUsername != "" {
			opts.SetUsername(rootFlags.mqttUsername)
		}
		if rootFlags.mqttPassword != "" {
			opts.SetPassword(rootFlags.mqttPassword)
		}
		opts.SetConnectRetry(true)
		opts.SetConnectRetryInterval(10 * time.Second)
		mqttClient = mqtt.NewClient(opts)
		if token := mqttClient.Connect(); token.WaitTimeout(5*time.Second) && token.Error() != nil {
			log.Fatalf("cannot connect to MQTT broker: %v", token.Error())
		}
	}

	ctx, cancel := context.WithCancel(context.Background())
	signals := make(chan os.Signal, 1)
	signal.Notify(signals, syscall.SIGHUP, syscall.SIGINT, syscall.SIGTERM, syscall.SIGQUIT)
	go handleCancelation(signals, cancel)

	meter := &NoiseMeter{
		trx:          rootFlags.trx,
		mqttClient:   mqttClient,
		mqttTopic:    rootFlags.mqttTopic,
		filename:     rootFlags.filename,
		scanInterval: rootFlags.scanInterval,
		samples:      make(chan []float32, 1),
		done:         ctx.Done(),
	}

	var tciClient *tci.Client
	tciClient = tci.KeepOpen(tciHost, 10*time.Second, tci.ConnectionListenerFunc(func(connected bool) {
		tciClient.StartIQ(rootFlags.trx)
	}), meter)
	defer tciClient.StopIQ(rootFlags.trx)
	go meter.run()

	<-ctx.Done()
}

type NoiseMeter struct {
	trx          int
	mqttClient   mqtt.Client
	mqttTopic    string
	filename     string
	scanInterval time.Duration

	samples chan []float32
	done    <-chan struct{}
}

func (m *NoiseMeter) run() {
	ticker := time.NewTicker(m.scanInterval)
	defer ticker.Stop()
	consumeNextSampleBatch := false
	for {
		select {
		case <-m.done:
			return
		case <-ticker.C:
			consumeNextSampleBatch = true
		case samples := <-m.samples:
			if !consumeNextSampleBatch {
				continue
			}
			consumeNextSampleBatch = false
			now := time.Now()
			csamples := make([]complex128, len(samples)/2)
			for i := range csamples {
				csamples[i] = complex(float64(samples[i*2]), float64(samples[i*2+1]))
			}
			fft := dsp.FFT(csamples)
			var mean float64
			magnitude := make([]float64, len(fft))
			for i, tap := range fft {
				magnitude[i] = 10.0 * math.Log10(20.0*(math.Pow(real(tap), 2)+math.Pow(imag(tap), 2))/math.Pow(float64(len(fft)), 2))
				mean += magnitude[i]
			}
			mean /= float64(len(fft))
			var sigma float64
			for _, x := range magnitude {
				sigma += math.Pow(x-mean, 2)
			}
			sigma = math.Sqrt(sigma / float64(len(magnitude)))

			m.PublishToMQTT(now, mean, sigma)
			m.LogToFile(now, mean, sigma)

			log.Printf("Noise %s m:%5.1f s:%f\n", m.mqttTopic, mean, sigma)
		}
	}
}

func (m *NoiseMeter) PublishToMQTT(timestamp time.Time, mean float64, sigma float64) {
	if m.mqttClient == nil {
		return
	}
	timestampStr := timestamp.UTC().Format(time.RFC3339)
	token := m.mqttClient.Publish(m.mqttTopic, 0, false, fmt.Sprintf(`{"timestamp":"%s", "noise_level":%.1f, "sigma": %.1f}`, timestampStr, mean, sigma))
	if token.WaitTimeout(1*time.Second) && token.Error() != nil {
		log.Printf("cannot publish: %v", token.Error())
		return
	}
}

func (m *NoiseMeter) LogToFile(timestamp time.Time, mean float64, sigma float64) {
	if m.filename == "" {
		return
	}

	f, err := os.OpenFile(m.filename, os.O_APPEND|os.O_WRONLY|os.O_CREATE, 0600)
	if err != nil {
		log.Printf("cannot open log file: %v", err)
		return
	}
	defer f.Close()

	timestampStr := timestamp.UTC().Format(time.RFC3339)
	_, err = fmt.Fprintf(f, "%s;%.1f;%.1f\n", timestampStr, mean, sigma)
	if err != nil {
		log.Printf("cannot log to file: %v", err)
		return
	}
}

func (m *NoiseMeter) IQData(trx int, sampleRate tci.IQSampleRate, samples []float32) {
	if trx != m.trx {
		return
	}
	m.samples <- samples
}

func handleCancelation(signals <-chan os.Signal, cancel context.CancelFunc) {
	count := 0
	for {
		select {
		case <-signals:
			count++
			if count == 1 {
				cancel()
			} else {
				log.Fatal("hard shutdown")
			}
		}
	}
}

func parseTCPAddrArg(arg string, defaultHost string, defaultPort int) (*net.TCPAddr, error) {
	host, port := splitHostPort(arg)
	if host == "" {
		host = defaultHost
	}
	if port == "" {
		port = strconv.Itoa(defaultPort)
	}

	return net.ResolveTCPAddr("tcp", fmt.Sprintf("%s:%s", host, port))
}

func splitHostPort(hostport string) (host, port string) {
	host = hostport

	colon := strings.LastIndexByte(host, ':')
	if colon != -1 && validOptionalPort(host[colon:]) {
		host, port = host[:colon], host[colon+1:]
	}

	if strings.HasPrefix(host, "[") && strings.HasSuffix(host, "]") {
		host = host[1 : len(host)-1]
	}

	return
}

func validOptionalPort(port string) bool {
	if port == "" {
		return true
	}
	if port[0] != ':' {
		return false
	}
	for _, b := range port[1:] {
		if b < '0' || b > '9' {
			return false
		}
	}
	return true
}
