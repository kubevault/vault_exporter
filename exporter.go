// Copyright 2013 The Prometheus Authors
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
// http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package main

import (
	"bufio"
	"fmt"
	"io"
	"net"
	"os"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/go-kit/kit/log"
	"github.com/go-kit/kit/log/level"
	"github.com/prometheus/client_golang/prometheus"

	"kubevault.dev/vault_exporter/pkg/clock"
	"kubevault.dev/vault_exporter/pkg/mapper"
)

const (
	defaultHelp = "Metric autogenerated by statsd_exporter."
	regErrF     = "Failed to update metric"
)

// uncheckedCollector wraps a Collector but its Describe method yields no Desc.
// This allows incoming metrics to have inconsistent label sets
type uncheckedCollector struct {
	c prometheus.Collector
}

func (u uncheckedCollector) Describe(_ chan<- *prometheus.Desc) {}
func (u uncheckedCollector) Collect(c chan<- prometheus.Metric) {
	u.c.Collect(c)
}

type Exporter struct {
	mapper   *mapper.MetricMapper
	registry *registry
	logger   log.Logger
}

// Replace invalid characters in the metric name with "_"
// Valid characters are a-z, A-Z, 0-9, and _
func escapeMetricName(metricName string) string {
	metricLen := len(metricName)
	if metricLen == 0 {
		return ""
	}

	escaped := false
	var sb strings.Builder
	// If a metric starts with a digit, allocate the memory and prepend an
	// underscore.
	if metricName[0] >= '0' && metricName[0] <= '9' {
		escaped = true
		sb.Grow(metricLen + 1)
		sb.WriteByte('_')
	}

	// This is an character replacement method optimized for this limited
	// use case.  It is much faster than using a regex.
	offset := 0
	for i, c := range metricName {
		// Seek forward, skipping valid characters until we find one that needs
		// to be replaced, then add all the characters we've seen so far to the
		// string.Builder.
		if (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') ||
			(c >= '0' && c <= '9') || (c == '_') {
			// Character is valid, so skip over it without doing anything.
		} else {
			if !escaped {
				// Up until now we've been lazy and avoided actually allocating
				// memory.  Unfortunately we've now determined this string needs
				// escaping, so allocate the buffer for the whole string.
				escaped = true
				sb.Grow(metricLen)
			}
			sb.WriteString(metricName[offset:i])
			offset = i + utf8.RuneLen(c)
			sb.WriteByte('_')
		}
	}

	if !escaped {
		// This is the happy path where nothing had to be escaped, so we can
		// avoid doing anything.
		return metricName
	}

	if offset < metricLen {
		sb.WriteString(metricName[offset:])
	}

	return sb.String()
}

// Listen handles all events sent to the given channel sequentially. It
// terminates when the channel is closed.
func (b *Exporter) Listen(e <-chan Events) {
	removeStaleMetricsTicker := clock.NewTicker(time.Second)

	for {
		select {
		case <-removeStaleMetricsTicker.C:
			b.registry.removeStaleMetrics()
		case events, ok := <-e:
			if !ok {
				level.Debug(b.logger).Log("msg", "Channel is closed. Break out of Exporter.Listener.")
				removeStaleMetricsTicker.Stop()
				return
			}
			for _, event := range events {
				b.handleEvent(event)
			}
		}
	}
}

// handleEvent processes a single Event according to the configured mapping.
func (b *Exporter) handleEvent(event Event) {
	mapping, labels, present := b.mapper.GetMapping(event.MetricName(), event.MetricType())
	if mapping == nil {
		mapping = &mapper.MetricMapping{}
		if b.mapper.Defaults.Ttl != 0 {
			mapping.Ttl = b.mapper.Defaults.Ttl
		}
	}

	if mapping.Action == mapper.ActionTypeDrop {
		eventsActions.WithLabelValues("drop").Inc()
		return
	}

	help := defaultHelp
	if mapping.HelpText != "" {
		help = mapping.HelpText
	}

	metricName := ""
	prometheusLabels := event.Labels()
	if present {
		if mapping.Name == "" {
			level.Debug(b.logger).Log("msg", "The mapping generates an empty metric name", "metric_name", event.MetricName(), "match", mapping.Match)
			errorEventStats.WithLabelValues("empty_metric_name").Inc()
			return
		}
		metricName = escapeMetricName(mapping.Name)
		for label, value := range labels {
			prometheusLabels[label] = value
		}
		eventsActions.WithLabelValues(string(mapping.Action)).Inc()
	} else {
		eventsUnmapped.Inc()
		metricName = escapeMetricName(event.MetricName())
	}

	switch ev := event.(type) {
	case *CounterEvent:
		// We don't accept negative values for counters. Incrementing the counter with a negative number
		// will cause the exporter to panic. Instead we will warn and continue to the next event.
		if event.Value() < 0.0 {
			level.Debug(b.logger).Log("msg", "counter must be non-negative value", "metric", metricName, "event_value", event.Value())
			errorEventStats.WithLabelValues("illegal_negative_counter").Inc()
			return
		}

		counter, err := b.registry.getCounter(metricName, prometheusLabels, help, mapping)
		if err == nil {
			counter.Add(event.Value())
			eventStats.WithLabelValues("counter").Inc()
		} else {
			level.Debug(b.logger).Log("msg", regErrF, "metric", metricName, "error", err)
			conflictingEventStats.WithLabelValues("counter").Inc()
		}

	case *GaugeEvent:
		gauge, err := b.registry.getGauge(metricName, prometheusLabels, help, mapping)

		if err == nil {
			if ev.relative {
				gauge.Add(event.Value())
			} else {
				gauge.Set(event.Value())
			}
			eventStats.WithLabelValues("gauge").Inc()
		} else {
			level.Debug(b.logger).Log("msg", regErrF, "metric", metricName, "error", err)
			conflictingEventStats.WithLabelValues("gauge").Inc()
		}

	case *TimerEvent:
		t := mapper.TimerTypeDefault
		if mapping != nil {
			t = mapping.TimerType
		}
		if t == mapper.TimerTypeDefault {
			t = b.mapper.Defaults.TimerType
		}

		switch t {
		case mapper.TimerTypeHistogram:
			histogram, err := b.registry.getHistogram(metricName, prometheusLabels, help, mapping)
			if err == nil {
				histogram.Observe(event.Value() / 1000) // prometheus presumes seconds, statsd millisecond
				eventStats.WithLabelValues("timer").Inc()
			} else {
				level.Debug(b.logger).Log("msg", regErrF, "metric", metricName, "error", err)
				conflictingEventStats.WithLabelValues("timer").Inc()
			}

		case mapper.TimerTypeDefault, mapper.TimerTypeSummary:
			summary, err := b.registry.getSummary(metricName, prometheusLabels, help, mapping)
			if err == nil {
				summary.Observe(event.Value() / 1000) // prometheus presumes seconds, statsd millisecond
				eventStats.WithLabelValues("timer").Inc()
			} else {
				level.Debug(b.logger).Log("msg", regErrF, "metric", metricName, "error", err)
				conflictingEventStats.WithLabelValues("timer").Inc()
			}

		default:
			level.Error(b.logger).Log("msg", "unknown timer type", "type", t)
			os.Exit(1)
		}

	default:
		level.Debug(b.logger).Log("msg", "Unsupported event type")
		eventStats.WithLabelValues("illegal").Inc()
	}
}

func NewExporter(mapper *mapper.MetricMapper, logger log.Logger) *Exporter {
	return &Exporter{
		mapper:   mapper,
		registry: newRegistry(mapper),
		logger:   logger,
	}
}

func buildEvent(statType, metric string, value float64, relative bool, labels map[string]string) (Event, error) {
	switch statType {
	case "c":
		return &CounterEvent{
			metricName: metric,
			value:      float64(value),
			labels:     labels,
		}, nil
	case "g":
		return &GaugeEvent{
			metricName: metric,
			value:      float64(value),
			relative:   relative,
			labels:     labels,
		}, nil
	case "ms", "h", "d":
		return &TimerEvent{
			metricName: metric,
			value:      float64(value),
			labels:     labels,
		}, nil
	case "s":
		return nil, fmt.Errorf("no support for StatsD sets")
	default:
		return nil, fmt.Errorf("bad stat type %s", statType)
	}
}

func parseTag(component, tag string, separator rune, labels map[string]string, logger log.Logger) {
	// Entirely empty tag is an error
	if len(tag) == 0 {
		tagErrors.Inc()
		level.Debug(logger).Log("msg", "Empty name tag", "component", component)
		return
	}

	for i, c := range tag {
		if c == separator {
			k := tag[:i]
			v := tag[i+1:]

			if len(k) == 0 || len(v) == 0 {
				// Empty key or value is an error
				tagErrors.Inc()
				level.Debug(logger).Log("msg", "Malformed name tag", "k", k, "v", v, "component", component)
			} else {
				labels[escapeMetricName(k)] = v
			}
			return
		}
	}

	// Missing separator (no value) is an error
	tagErrors.Inc()
	level.Debug(logger).Log("msg", "Malformed name tag", "tag", tag, "component", component)
}

func parseNameTags(component string, labels map[string]string, logger log.Logger) {
	lastTagEndIndex := 0
	for i, c := range component {
		if c == ',' {
			tag := component[lastTagEndIndex:i]
			lastTagEndIndex = i + 1
			parseTag(component, tag, '=', labels, logger)
		}
	}

	// If we're not off the end of the string, add the last tag
	if lastTagEndIndex < len(component) {
		tag := component[lastTagEndIndex:]
		parseTag(component, tag, '=', labels, logger)
	}
}

func trimLeftHash(s string) string {
	if s != "" && s[0] == '#' {
		return s[1:]
	}
	return s
}

func parseDogStatsDTags(component string, labels map[string]string, logger log.Logger) {
	lastTagEndIndex := 0
	for i, c := range component {
		if c == ',' {
			tag := component[lastTagEndIndex:i]
			lastTagEndIndex = i + 1
			parseTag(component, trimLeftHash(tag), ':', labels, logger)
		}
	}

	// If we're not off the end of the string, add the last tag
	if lastTagEndIndex < len(component) {
		tag := component[lastTagEndIndex:]
		parseTag(component, trimLeftHash(tag), ':', labels, logger)
	}
}

func parseNameAndTags(name string, labels map[string]string, logger log.Logger) string {
	for i, c := range name {
		// `#` delimits start of tags by Librato
		// https://www.librato.com/docs/kb/collect/collection_agents/stastd/#stat-level-tags
		// `,` delimits start of tags by InfluxDB
		// https://www.influxdata.com/blog/getting-started-with-sending-statsd-metrics-to-telegraf-influxdb/#introducing-influx-statsd
		if c == '#' || c == ',' {
			parseNameTags(name[i+1:], labels, logger)
			return name[:i]
		}
	}
	return name
}

func lineToEvents(line string, logger log.Logger) Events {
	events := Events{}
	if line == "" {
		return events
	}

	elements := strings.SplitN(line, ":", 2)
	if len(elements) < 2 || len(elements[0]) == 0 || !utf8.ValidString(line) {
		sampleErrors.WithLabelValues("malformed_line").Inc()
		level.Debug(logger).Log("msg", "Bad line from StatsD", "line", line)
		return events
	}

	labels := map[string]string{}
	metric := parseNameAndTags(elements[0], labels, logger)

	var samples []string
	if strings.Contains(elements[1], "|#") {
		// using DogStatsD tags

		// don't allow mixed tagging styles
		if len(labels) > 0 {
			sampleErrors.WithLabelValues("mixed_tagging_styles").Inc()
			level.Debug(logger).Log("msg", "Bad line (multiple tagging styles) from StatsD", "line", line)
			return events
		}

		// disable multi-metrics
		samples = elements[1:]
	} else {
		samples = strings.Split(elements[1], ":")
	}
samples:
	for _, sample := range samples {
		samplesReceived.Inc()
		components := strings.Split(sample, "|")
		samplingFactor := 1.0
		if len(components) < 2 || len(components) > 4 {
			sampleErrors.WithLabelValues("malformed_component").Inc()
			level.Debug(logger).Log("msg", "Bad component", "line", line)
			continue
		}
		valueStr, statType := components[0], components[1]

		var relative = false
		if strings.Index(valueStr, "+") == 0 || strings.Index(valueStr, "-") == 0 {
			relative = true
		}

		value, err := strconv.ParseFloat(valueStr, 64)
		if err != nil {
			level.Debug(logger).Log("msg", "Bad value", "value", valueStr, "line", line)
			sampleErrors.WithLabelValues("malformed_value").Inc()
			continue
		}

		multiplyEvents := 1
		if len(components) >= 3 {
			for _, component := range components[2:] {
				if len(component) == 0 {
					level.Debug(logger).Log("msg", "Empty component", "line", line)
					sampleErrors.WithLabelValues("malformed_component").Inc()
					continue samples
				}
			}

			for _, component := range components[2:] {
				switch component[0] {
				case '@':

					samplingFactor, err = strconv.ParseFloat(component[1:], 64)
					if err != nil {
						level.Debug(logger).Log("msg", "Invalid sampling factor", "component", component[1:], "line", line)
						sampleErrors.WithLabelValues("invalid_sample_factor").Inc()
					}
					if samplingFactor == 0 {
						samplingFactor = 1
					}

					if statType == "g" {
						continue
					} else if statType == "c" {
						value /= samplingFactor
					} else if statType == "ms" || statType == "h" || statType == "d" {
						multiplyEvents = int(1 / samplingFactor)
					}
				case '#':
					parseDogStatsDTags(component[1:], labels, logger)
				default:
					level.Debug(logger).Log("msg", "Invalid sampling factor or tag section", "component", components[2], "line", line)
					sampleErrors.WithLabelValues("invalid_sample_factor").Inc()
					continue
				}
			}
		}

		if len(labels) > 0 {
			tagsReceived.Inc()
		}

		for i := 0; i < multiplyEvents; i++ {
			event, err := buildEvent(statType, metric, value, relative, labels)
			if err != nil {
				level.Debug(logger).Log("msg", "Error building event", "line", line, "error", err)
				sampleErrors.WithLabelValues("illegal_event").Inc()
				continue
			}
			events = append(events, event)
		}
	}
	return events
}

type StatsDUDPListener struct {
	conn         *net.UDPConn
	eventHandler eventHandler
	logger       log.Logger
}

func (l *StatsDUDPListener) SetEventHandler(eh eventHandler) {
	l.eventHandler = eh
}

func (l *StatsDUDPListener) Listen() {
	buf := make([]byte, 65535)
	for {
		n, _, err := l.conn.ReadFromUDP(buf)
		if err != nil {
			// https://github.com/golang/go/issues/4373
			// ignore net: errClosing error as it will occur during shutdown
			if strings.HasSuffix(err.Error(), "use of closed network connection") {
				return
			}
			level.Error(l.logger).Log("error", err)
			return
		}
		l.handlePacket(buf[0:n])
	}
}

func (l *StatsDUDPListener) handlePacket(packet []byte) {
	udpPackets.Inc()
	lines := strings.Split(string(packet), "\n")
	for _, line := range lines {
		linesReceived.Inc()
		l.eventHandler.queue(lineToEvents(line, l.logger))
	}
}

type StatsDTCPListener struct {
	conn         *net.TCPListener
	eventHandler eventHandler
	logger       log.Logger
}

func (l *StatsDTCPListener) SetEventHandler(eh eventHandler) {
	l.eventHandler = eh
}

func (l *StatsDTCPListener) Listen() {
	for {
		c, err := l.conn.AcceptTCP()
		if err != nil {
			// https://github.com/golang/go/issues/4373
			// ignore net: errClosing error as it will occur during shutdown
			if strings.HasSuffix(err.Error(), "use of closed network connection") {
				return
			}
			level.Error(l.logger).Log("msg", "AcceptTCP failed", "error", err)
			os.Exit(1)
		}
		go l.handleConn(c)
	}
}

func (l *StatsDTCPListener) handleConn(c *net.TCPConn) {
	defer c.Close()

	tcpConnections.Inc()

	r := bufio.NewReader(c)
	for {
		line, isPrefix, err := r.ReadLine()
		if err != nil {
			if err != io.EOF {
				tcpErrors.Inc()
				level.Debug(l.logger).Log("msg", "Read failed", "addr", c.RemoteAddr(), "error", err)
			}
			break
		}
		if isPrefix {
			tcpLineTooLong.Inc()
			level.Debug(l.logger).Log("msg", "Read failed: line too long", "addr", c.RemoteAddr())
			break
		}
		linesReceived.Inc()
		l.eventHandler.queue(lineToEvents(string(line), l.logger))
	}
}

type StatsDUnixgramListener struct {
	conn         *net.UnixConn
	eventHandler eventHandler
	logger       log.Logger
}

func (l *StatsDUnixgramListener) SetEventHandler(eh eventHandler) {
	l.eventHandler = eh
}

func (l *StatsDUnixgramListener) Listen() {
	buf := make([]byte, 65535)
	for {
		n, _, err := l.conn.ReadFromUnix(buf)
		if err != nil {
			// https://github.com/golang/go/issues/4373
			// ignore net: errClosing error as it will occur during shutdown
			if strings.HasSuffix(err.Error(), "use of closed network connection") {
				return
			}
			level.Error(l.logger).Log(err)
			os.Exit(1)
		}
		l.handlePacket(buf[:n])
	}
}

func (l *StatsDUnixgramListener) handlePacket(packet []byte) {
	unixgramPackets.Inc()
	lines := strings.Split(string(packet), "\n")
	for _, line := range lines {
		linesReceived.Inc()
		l.eventHandler.queue(lineToEvents(string(line), l.logger))
	}
}
