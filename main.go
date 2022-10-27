// Copyright 2020 the generic-device-plugin authors
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path"
	"regexp"
	"strings"
	"syscall"

	"github.com/go-kit/kit/log"
	"github.com/go-kit/kit/log/level"
	"github.com/oklog/run"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	flag "github.com/spf13/pflag"
	"k8s.io/apimachinery/pkg/util/validation"
	"k8s.io/kubelet/pkg/apis/deviceplugin/v1beta1"

	"github.com/squat/generic-device-plugin/deviceplugin"
	"github.com/squat/generic-device-plugin/version"
)

const (
	logLevelAll   = "all"
	logLevelDebug = "debug"
	logLevelInfo  = "info"
	logLevelWarn  = "warn"
	logLevelError = "error"
	logLevelNone  = "none"

	defaultDomain = "tobilehman.com"
)

var (
	availableLogLevels = strings.Join([]string{
		logLevelAll,
		logLevelDebug,
		logLevelInfo,
		logLevelWarn,
		logLevelError,
		logLevelNone,
	}, ", ")
)

// Main is the principal function for the binary, wrapped only by `main` for convenience.
func Main() error {
	domain := flag.String("domain", defaultDomain, "The domain to use when when declaring devices.")
	deviceSpecsRaw := flag.StringArray("device", nil, `The devices to expose. This flag can be repeated to specify multiple device types.
Multiple paths can be given for each type. Paths can be globs.
Should be provided in the form:
{"name": "<name>", "groups": [{"paths": [{"path": "<path-1>", "mountPath": "<mount-path-1>"},{"path": "<path-2>", "mountPath": "<mount-path-2>"}], "count": <count>}]}
For example, to expose serial devices with different names: {"name": "serial", "groups": [{"paths": [{"path": "/dev/ttyUSB*"}]}, {"paths": [{"path": "/dev/ttyACM*"}]}]}
Paths can contain lists of devices that should be grouped and mounted into a container together as one single meta-device.
For example, to allocate and mount an audio capture device: {"name": "capture", "groups": [{"paths": [{"path": "/dev/snd/pcmC0D0c"}, {"path": "/dev/snd/controlC0"}]}]}
A "count" can be specified to allow a discovered device to be scheduled multiple times.
Note: if omitted, "count" is assumed to be 1`)
	pluginPath := flag.String("plugin-directory", v1beta1.DevicePluginPath, "The directory in which to create plugin sockets.")
	logLevel := flag.String("log-level", logLevelInfo, fmt.Sprintf("Log level to use. Possible values: %s", availableLogLevels))
	listen := flag.String("listen", ":8080", "The address at which to listen for health and metrics.")
	printVersion := flag.Bool("version", false, "Print version and exit")
	flag.Parse()

	if *printVersion {
		fmt.Println(version.Version)
		return nil
	}

	if errs := validation.IsDNS1123Subdomain(*domain); len(errs) > 0 {
		return fmt.Errorf("failed to parse domain %q: %s", *domain, strings.Join(errs, ", "))
	}

	deviceTypeFmt := "[a-z0-9][-a-z0-9]*[a-z0-9]"
	deviceTypeRegexp := regexp.MustCompile("^" + deviceTypeFmt + "$")
	var trim string
	deviceSpecs := make([]*deviceplugin.DeviceSpec, len(*deviceSpecsRaw))
	for i, dsr := range *deviceSpecsRaw {
		if err := json.Unmarshal([]byte(dsr), &deviceSpecs[i]); err != nil {
			return fmt.Errorf("failed to parse device %q; device must be specified in the form {\"type\": \"<type>\", \"count\": <count>, \"paths\": [\"<path-0>\",\"<path-1>\",\"<path-x>\"]}", dsr)
		}
		// Apply defaults.
		deviceSpecs[i].Default()
		trim = strings.TrimSpace(deviceSpecs[i].Name)
		if !deviceTypeRegexp.MatchString(trim) {
			return fmt.Errorf("failed to parse device %q; device type must match the regular expression %q", dsr, deviceTypeFmt)
		}
		deviceSpecs[i].Name = path.Join(*domain, trim)
		for j := range deviceSpecs[i].Groups {
			for k := range deviceSpecs[i].Groups[j].Paths {
				deviceSpecs[i].Groups[j].Paths[k].Path = strings.TrimSpace(deviceSpecs[i].Groups[j].Paths[k].Path)
				deviceSpecs[i].Groups[j].Paths[k].MountPath = strings.TrimSpace(deviceSpecs[i].Groups[j].Paths[k].MountPath)
			}
		}
	}
	if len(deviceSpecs) == 0 {
		return fmt.Errorf("at least one device must be specified")
	}

	logger := log.NewJSONLogger(log.NewSyncWriter(os.Stdout))
	switch *logLevel {
	case logLevelAll:
		logger = level.NewFilter(logger, level.AllowAll())
	case logLevelDebug:
		logger = level.NewFilter(logger, level.AllowDebug())
	case logLevelInfo:
		logger = level.NewFilter(logger, level.AllowInfo())
	case logLevelWarn:
		logger = level.NewFilter(logger, level.AllowWarn())
	case logLevelError:
		logger = level.NewFilter(logger, level.AllowError())
	case logLevelNone:
		logger = level.NewFilter(logger, level.AllowNone())
	default:
		return fmt.Errorf("log level %v unknown; possible values are: %s", *logLevel, availableLogLevels)
	}
	logger = log.With(logger, "ts", log.DefaultTimestampUTC)
	logger = log.With(logger, "caller", log.DefaultCaller)

	r := prometheus.NewRegistry()
	r.MustRegister(
		prometheus.NewGoCollector(),
		prometheus.NewProcessCollector(prometheus.ProcessCollectorOpts{}),
	)

	var g run.Group
	{
		// Run the HTTP server.
		mux := http.NewServeMux()
		mux.HandleFunc("/health", func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusOK)
		})
		mux.Handle("/metrics", promhttp.HandlerFor(r, promhttp.HandlerOpts{}))
		l, err := net.Listen("tcp", *listen)
		if err != nil {
			return fmt.Errorf("failed to listen on %s: %v", *listen, err)
		}

		g.Add(func() error {
			if err := http.Serve(l, mux); err != nil && err != http.ErrServerClosed {
				return fmt.Errorf("server exited unexpectedly: %v", err)
			}
			return nil
		}, func(error) {
			l.Close()
		})
	}

	{
		// Exit gracefully on SIGINT and SIGTERM.
		term := make(chan os.Signal, 1)
		signal.Notify(term, syscall.SIGINT, syscall.SIGTERM)
		cancel := make(chan struct{})
		g.Add(func() error {
			for {
				select {
				case <-term:
					logger.Log("msg", "caught interrupt; gracefully cleaning up; see you next time!")
					return nil
				case <-cancel:
					return nil
				}
			}
		}, func(error) {
			close(cancel)
		})
	}

	for i := range deviceSpecs {
		d := deviceSpecs[i]
		ctx, cancel := context.WithCancel(context.Background())
		gp := deviceplugin.NewGenericPlugin(d, *pluginPath, log.With(logger, "resource", d.Name), prometheus.WrapRegistererWith(prometheus.Labels{"resource": d.Name}, r))
		// Start the generic device plugin server.
		g.Add(func() error {
			logger.Log("msg", fmt.Sprintf("Starting the generic-device-plugin for %q.", d.Name))
			return gp.Run(ctx)
		}, func(error) {
			cancel()
		})
	}

	return g.Run()
}

func main() {
	if err := Main(); err != nil {
		fmt.Fprintf(os.Stderr, "%v\n", err)
		os.Exit(1)
	}
}
