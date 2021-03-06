// Package splunk provides the log driver for forwarding server logs to
// Splunk HTTP Event Collector endpoint.
package main

import (
	"bytes"
	"compress/gzip"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/binary"
	"fmt"
	"io"
	"io/ioutil"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"sync"
	"syscall"
	"time"

	"encoding/json"
	"strings"

	"github.com/Sirupsen/logrus"
	"github.com/docker/docker/api/types/plugins/logdriver"
	"github.com/docker/docker/daemon/logger"
	"github.com/docker/docker/daemon/logger/jsonfilelog"
	"github.com/docker/docker/daemon/logger/loggerutils"
	"github.com/docker/docker/pkg/urlutil"
	protoio "github.com/gogo/protobuf/io"
	"github.com/pkg/errors"
	"github.com/tonistiigi/fifo"
)

const (
	driverName                    = "splunk"
	splunkURLKey                  = "splunk-url"
	splunkURLPathKey              = "splunk-url-path"
	splunkTokenKey                = "splunk-token"
	splunkSourceKey               = "splunk-source"
	splunkSourceTypeKey           = "splunk-sourcetype"
	splunkIndexKey                = "splunk-index"
	splunkCAPathKey               = "splunk-capath"
	splunkCANameKey               = "splunk-caname"
	splunkInsecureSkipVerifyKey   = "splunk-insecureskipverify"
	splunkFormatKey               = "splunk-format"
	splunkVerifyConnectionKey     = "splunk-verify-connection"
	splunkGzipCompressionKey      = "splunk-gzip"
	splunkGzipCompressionLevelKey = "splunk-gzip-level"
	envKey                        = "env"
	envRegexKey                   = "env-regex"
	labelsKey                     = "labels"
	tagKey                        = "tag"
)

const (
	// How often do we send messages (if we are not reaching batch size)
	defaultPostMessagesFrequency = 5 * time.Second
	// How big can be batch of messages
	defaultPostMessagesBatchSize = 1000
	// Maximum number of messages we can store in buffer
	defaultBufferMaximum = 10 * defaultPostMessagesBatchSize
	// Number of messages allowed to be queued in the channel
	defaultStreamChannelSize = 4 * defaultPostMessagesBatchSize
)

const (
	envVarPostMessagesFrequency = "SPLUNK_LOGGING_DRIVER_POST_MESSAGES_FREQUENCY"
	envVarPostMessagesBatchSize = "SPLUNK_LOGGING_DRIVER_POST_MESSAGES_BATCH_SIZE"
	envVarBufferMaximum         = "SPLUNK_LOGGING_DRIVER_BUFFER_MAX"
	envVarStreamChannelSize     = "SPLUNK_LOGGING_DRIVER_CHANNEL_SIZE"
)

type splunkLoggerInterface interface {
	logger.Logger
	worker()
}

type splunkLogger struct {
	client    *http.Client
	transport *http.Transport

	url         string
	auth        string
	nullMessage *splunkMessage

	// http compression
	gzipCompression      bool
	gzipCompressionLevel int

	// Advanced options
	postMessagesFrequency time.Duration
	postMessagesBatchSize int
	bufferMaximum         int

	// For synchronization between background worker and logger.
	// We use channel to send messages to worker go routine.
	// All other variables for blocking Close call before we flush all messages to HEC
	stream     chan *splunkMessage
	lock       sync.RWMutex
	closed     bool
	closedCond *sync.Cond
}

type splunkLoggerInline struct {
	*splunkLogger

	nullEvent *splunkMessageEvent
}

type splunkLoggerNova struct {
	*splunkLogger

	prefix []byte
}

type splunkLoggerJSON struct {
	*splunkLoggerInline
}

type splunkLoggerRaw struct {
	*splunkLogger

	prefix []byte
}

type splunkMessage struct {
	Event      interface{} `json:"event"`
	Time       string      `json:"time"`
	Host       string      `json:"host"`
	Source     string      `json:"source,omitempty"`
	SourceType string      `json:"sourcetype,omitempty"`
	Index      string      `json:"index,omitempty"`
	Entity     string      `json:"entity,omitempty"`
}

type splunkMessageEvent struct {
	Line   interface{}       `json:"line"`
	Source string            `json:"source"`
	Tag    string            `json:"tag,omitempty"`
	Attrs  map[string]string `json:"attrs,omitempty"`
}

const (
	splunkFormatRaw    = "raw"
	splunkFormatJSON   = "json"
	splunkFormatInline = "inline"
	splunkFormatNova   = "nova"
)

// New creates splunk logger driver using configuration passed in context
func New(info logger.Info) (logger.Logger, error) {
	hostname, err := info.Hostname()
	if err != nil {
		return nil, fmt.Errorf("%s: cannot access hostname to set source field", driverName)
	}

	// Parse and validate Splunk URL
	splunkURL, err := parseURL(info)
	if err != nil {
		return nil, err
	}

	// Splunk Token is required parameter
	splunkToken, ok := info.Config[splunkTokenKey]
	if !ok {
		return nil, fmt.Errorf("%s: %s is expected", driverName, splunkTokenKey)
	}

	tlsConfig := &tls.Config{}

	// Splunk is using autogenerated certificates by default,
	// allow users to trust them with skipping verification
	if insecureSkipVerifyStr, ok := info.Config[splunkInsecureSkipVerifyKey]; ok {
		insecureSkipVerify, err := strconv.ParseBool(insecureSkipVerifyStr)
		if err != nil {
			return nil, err
		}
		tlsConfig.InsecureSkipVerify = insecureSkipVerify
	}

	// If path to the root certificate is provided - load it
	if caPath, ok := info.Config[splunkCAPathKey]; ok {
		caCert, err := ioutil.ReadFile(caPath)
		if err != nil {
			return nil, err
		}
		caPool := x509.NewCertPool()
		caPool.AppendCertsFromPEM(caCert)
		tlsConfig.RootCAs = caPool
	}

	if caName, ok := info.Config[splunkCANameKey]; ok {
		tlsConfig.ServerName = caName
	}

	gzipCompression := false
	if gzipCompressionStr, ok := info.Config[splunkGzipCompressionKey]; ok {
		gzipCompression, err = strconv.ParseBool(gzipCompressionStr)
		if err != nil {
			return nil, err
		}
	}

	gzipCompressionLevel := gzip.DefaultCompression
	if gzipCompressionLevelStr, ok := info.Config[splunkGzipCompressionLevelKey]; ok {
		var err error
		gzipCompressionLevel64, err := strconv.ParseInt(gzipCompressionLevelStr, 10, 32)
		if err != nil {
			return nil, err
		}
		gzipCompressionLevel = int(gzipCompressionLevel64)
		if gzipCompressionLevel < gzip.DefaultCompression || gzipCompressionLevel > gzip.BestCompression {
			err := fmt.Errorf("Not supported level '%s' for %s (supported values between %d and %d).",
				gzipCompressionLevelStr, splunkGzipCompressionLevelKey, gzip.DefaultCompression, gzip.BestCompression)
			return nil, err
		}
	}

	transport := &http.Transport{
		TLSClientConfig: tlsConfig,
	}
	client := &http.Client{
		Transport: transport,
	}

	source := info.Config[splunkSourceKey]
	sourceType := info.Config[splunkSourceTypeKey]
	index := info.Config[splunkIndexKey]

	var nullMessage = &splunkMessage{
		Host:       hostname,
		Source:     source,
		SourceType: sourceType,
		Index:      index,
	}

	// Allow user to remove tag from the messages by setting tag to empty string
	tag := ""
	if tagTemplate, ok := info.Config[tagKey]; !ok || tagTemplate != "" {
		tag, err = loggerutils.ParseLogTag(info, loggerutils.DefaultTemplate)
		if err != nil {
			return nil, err
		}
	}

	attrs, err := info.ExtraAttributes(nil)
	if err != nil {
		return nil, err
	}

	var (
		postMessagesFrequency = getAdvancedOptionDuration(envVarPostMessagesFrequency, defaultPostMessagesFrequency)
		postMessagesBatchSize = getAdvancedOptionInt(envVarPostMessagesBatchSize, defaultPostMessagesBatchSize)
		bufferMaximum         = getAdvancedOptionInt(envVarBufferMaximum, defaultBufferMaximum)
		streamChannelSize     = getAdvancedOptionInt(envVarStreamChannelSize, defaultStreamChannelSize)
	)

	logger := &splunkLogger{
		client:                client,
		transport:             transport,
		url:                   splunkURL.String(),
		auth:                  "Splunk " + splunkToken,
		nullMessage:           nullMessage,
		gzipCompression:       gzipCompression,
		gzipCompressionLevel:  gzipCompressionLevel,
		stream:                make(chan *splunkMessage, streamChannelSize),
		postMessagesFrequency: postMessagesFrequency,
		postMessagesBatchSize: postMessagesBatchSize,
		bufferMaximum:         bufferMaximum,
	}

	// By default we verify connection, but we allow use to skip that
	verifyConnection := true
	if verifyConnectionStr, ok := info.Config[splunkVerifyConnectionKey]; ok {
		var err error
		verifyConnection, err = strconv.ParseBool(verifyConnectionStr)
		if err != nil {
			return nil, err
		}
	}
	if verifyConnection {
		err = verifySplunkConnection(logger)
		if err != nil {
			return nil, err
		}
	}

	var splunkFormat string
	if splunkFormatParsed, ok := info.Config[splunkFormatKey]; ok {
		switch splunkFormatParsed {
		case splunkFormatInline:
		case splunkFormatJSON:
		case splunkFormatRaw:
		case splunkFormatNova:
		default:
			return nil, fmt.Errorf("Unknown format specified %s, supported formats are inline, json, raw, and nova", splunkFormat)
		}
		splunkFormat = splunkFormatParsed
	} else {
		splunkFormat = splunkFormatInline
	}

	var loggerWrapper splunkLoggerInterface

	switch splunkFormat {
	case splunkFormatInline:
		nullEvent := &splunkMessageEvent{
			Tag:   tag,
			Attrs: attrs,
		}

		loggerWrapper = &splunkLoggerInline{logger, nullEvent}
	case splunkFormatJSON:
		nullEvent := &splunkMessageEvent{
			Tag:   tag,
			Attrs: attrs,
		}

		loggerWrapper = &splunkLoggerJSON{&splunkLoggerInline{logger, nullEvent}}
	case splunkFormatRaw:
		var prefix bytes.Buffer
		if tag != "" {
			prefix.WriteString(tag)
			prefix.WriteString(" ")
		}
		for key, value := range attrs {
			prefix.WriteString(key)
			prefix.WriteString("=")
			prefix.WriteString(value)
			prefix.WriteString(" ")
		}

		loggerWrapper = &splunkLoggerRaw{logger, prefix.Bytes()}
	case splunkFormatNova:
		var prefix bytes.Buffer
		if tag != "" {
			prefix.WriteString(tag)
			prefix.WriteString(" ")
		}
		for key, value := range attrs {
			prefix.WriteString(key)
			prefix.WriteString("=")
			prefix.WriteString(value)
			prefix.WriteString(" ")
		}

		loggerWrapper = &splunkLoggerNova{logger, prefix.Bytes()}
	default:
		return nil, fmt.Errorf("Unexpected format %s", splunkFormat)
	}

	go loggerWrapper.worker()

	return loggerWrapper, nil
}

func (l *splunkLoggerInline) Log(msg *logger.Message) error {
	message := l.createSplunkMessage(msg)

	event := *l.nullEvent
	event.Line = string(msg.Line)
	event.Source = msg.Source

	message.Event = &event
	logger.PutMessage(msg)
	return l.queueMessageAsync(message)
}

func (l *splunkLoggerNova) Log(msg *logger.Message) error {
	message := l.createSplunkMessage(msg)
	message.Entity = message.Host
	message.Source = message.Source

	message.Event = string(append(l.prefix, msg.Line...))
	logger.PutMessage(msg)
	return l.queueMessageAsync(message)
}

func (l *splunkLoggerJSON) Log(msg *logger.Message) error {
	message := l.createSplunkMessage(msg)
	event := *l.nullEvent

	var rawJSONMessage json.RawMessage
	if err := json.Unmarshal(msg.Line, &rawJSONMessage); err == nil {
		event.Line = &rawJSONMessage
	} else {
		event.Line = string(msg.Line)
	}

	event.Source = msg.Source

	message.Event = &event
	logger.PutMessage(msg)
	return l.queueMessageAsync(message)
}

func (l *splunkLoggerRaw) Log(msg *logger.Message) error {
	message := l.createSplunkMessage(msg)

	message.Event = string(append(l.prefix, msg.Line...))
	logger.PutMessage(msg)
	return l.queueMessageAsync(message)
}

func (l *splunkLogger) queueMessageAsync(message *splunkMessage) error {
	l.lock.RLock()
	defer l.lock.RUnlock()
	if l.closedCond != nil {
		return fmt.Errorf("%s: driver is closed", driverName)
	}
	l.stream <- message
	return nil
}

func (l *splunkLogger) worker() {
	timer := time.NewTicker(l.postMessagesFrequency)
	var messages []*splunkMessage
	for {
		select {
		case message, open := <-l.stream:
			if !open {
				l.postMessages(messages, true)
				l.lock.Lock()
				defer l.lock.Unlock()
				l.transport.CloseIdleConnections()
				l.closed = true
				l.closedCond.Signal()
				return
			}
			messages = append(messages, message)
			// Only sending when we get exactly to the batch size,
			// This also helps not to fire postMessages on every new message,
			// when previous try failed.
			if len(messages)%l.postMessagesBatchSize == 0 {
				messages = l.postMessages(messages, false)
			}
		case <-timer.C:
			messages = l.postMessages(messages, false)
		}
	}
}

func (l *splunkLogger) postMessages(messages []*splunkMessage, lastChance bool) []*splunkMessage {
	messagesLen := len(messages)
	for i := 0; i < messagesLen; i += l.postMessagesBatchSize {
		upperBound := i + l.postMessagesBatchSize
		if upperBound > messagesLen {
			upperBound = messagesLen
		}
		if err := l.tryPostMessages(messages[i:upperBound]); err != nil {
			logrus.Error(err)
			if messagesLen-i >= l.bufferMaximum || lastChance {
				// If this is last chance - print them all to the daemon log
				if lastChance {
					upperBound = messagesLen
				}
				// Not all sent, but buffer has got to its maximum, let's log all messages
				// we could not send and return buffer minus one batch size
				for j := i; j < upperBound; j++ {
					if jsonEvent, err := json.Marshal(messages[j]); err != nil {
						logrus.Error(err)
					} else {
						logrus.Error(fmt.Errorf("Failed to send a message '%s'", string(jsonEvent)))
					}
				}
				return messages[upperBound:messagesLen]
			}
			// Not all sent, returning buffer from where we have not sent messages
			return messages[i:messagesLen]
		}
	}
	// All sent, return empty buffer
	return messages[:0]
}

func (l *splunkLogger) tryPostMessages(messages []*splunkMessage) error {
	if len(messages) == 0 {
		return nil
	}
	var buffer bytes.Buffer
	var writer io.Writer
	var gzipWriter *gzip.Writer
	var err error
	// If gzip compression is enabled - create gzip writer with specified compression
	// level. If gzip compression is disabled, use standard buffer as a writer
	if l.gzipCompression {
		gzipWriter, err = gzip.NewWriterLevel(&buffer, l.gzipCompressionLevel)
		if err != nil {
			return err
		}
		writer = gzipWriter
	} else {
		writer = &buffer
	}
	for _, message := range messages {
		jsonEvent, err := json.Marshal(message)
		if err != nil {
			return err
		}
		if _, err := writer.Write(jsonEvent); err != nil {
			return err
		}
	}
	// If gzip compression is enabled, tell it, that we are done
	if l.gzipCompression {
		err = gzipWriter.Close()
		if err != nil {
			return err
		}
	}
	req, err := http.NewRequest("POST", l.url, bytes.NewBuffer(buffer.Bytes()))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", l.auth)
	// Tell if we are sending gzip compressed body
	if l.gzipCompression {
		req.Header.Set("Content-Encoding", "gzip")
	}
	res, err := l.client.Do(req)
	if err != nil {
		return err
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		var body []byte
		body, err = ioutil.ReadAll(res.Body)
		if err != nil {
			return err
		}
		return fmt.Errorf("%s: failed to send event - %s - %s", driverName, res.Status, body)
	}
	io.Copy(ioutil.Discard, res.Body)
	return nil
}

func (l *splunkLogger) Close() error {
	l.lock.Lock()
	defer l.lock.Unlock()
	if l.closedCond == nil {
		l.closedCond = sync.NewCond(&l.lock)
		close(l.stream)
		for !l.closed {
			l.closedCond.Wait()
		}
	}
	return nil
}

func (l *splunkLogger) Name() string {
	return driverName
}

func (l *splunkLogger) createSplunkMessage(msg *logger.Message) *splunkMessage {
	message := *l.nullMessage
	message.Time = fmt.Sprintf("%f", float64(msg.Timestamp.UnixNano())/float64(time.Second))
	return &message
}

// ValidateLogOpt looks for all supported by splunk driver options
func ValidateLogOpt(cfg map[string]string) error {
	for key := range cfg {
		switch key {
		case splunkURLKey:
		case splunkURLPathKey:
		case splunkTokenKey:
		case splunkSourceKey:
		case splunkSourceTypeKey:
		case splunkIndexKey:
		case splunkCAPathKey:
		case splunkCANameKey:
		case splunkInsecureSkipVerifyKey:
		case splunkFormatKey:
		case splunkVerifyConnectionKey:
		case splunkGzipCompressionKey:
		case splunkGzipCompressionLevelKey:
		case envKey:
		case envRegexKey:
		case labelsKey:
		case tagKey:
		default:
			return fmt.Errorf("unknown log opt '%s' for %s log driver", key, driverName)
		}
	}
	return nil
}

func parseURL(info logger.Info) (*url.URL, error) {
	splunkURLStr, ok := info.Config[splunkURLKey]
	if !ok {
		return nil, fmt.Errorf("%s: %s is expected", driverName, splunkURLKey)
	}

	splunkURL, err := url.Parse(splunkURLStr)
	if err != nil {
		return nil, fmt.Errorf("%s: failed to parse %s as url value in %s", driverName, splunkURLStr, splunkURLKey)
	}

	if !urlutil.IsURL(splunkURLStr) ||
		!splunkURL.IsAbs() ||
		(splunkURL.Path != "" && splunkURL.Path != "/") ||
		splunkURL.RawQuery != "" ||
		splunkURL.Fragment != "" {
		return nil, fmt.Errorf("%s: expected format scheme://dns_name_or_ip:port for %s", driverName, splunkURLKey)
	}

	splunkURLPathStr, ok := info.Config[splunkURLPathKey]
	if !ok {
		splunkURL.Path = "/services/collector/event/1.0"
	} else {
		if strings.HasPrefix(splunkURLPathStr, "/") {
			splunkURL.Path = splunkURLPathStr
		} else {
			return nil, fmt.Errorf("%s: expected format /path/to/collector for %s", driverName, splunkURLPathKey)
		}
	}

	return splunkURL, nil
}

func verifySplunkConnection(l *splunkLogger) error {
	req, err := http.NewRequest(http.MethodOptions, l.url, nil)
	if err != nil {
		return err
	}
	res, err := l.client.Do(req)
	if err != nil {
		return err
	}
	if res.Body != nil {
		defer res.Body.Close()
	}
	if res.StatusCode != http.StatusOK {
		var body []byte
		body, err = ioutil.ReadAll(res.Body)
		if err != nil {
			return err
		}
		return fmt.Errorf("%s: failed to verify connection - %s - %s", driverName, res.Status, body)
	}
	return nil
}

func getAdvancedOptionDuration(envName string, defaultValue time.Duration) time.Duration {
	valueStr := os.Getenv(envName)
	if valueStr == "" {
		return defaultValue
	}
	parsedValue, err := time.ParseDuration(valueStr)
	if err != nil {
		logrus.Error(fmt.Sprintf("Failed to parse value of %s as duration. Using default %v. %v", envName, defaultValue, err))
		return defaultValue
	}
	return parsedValue
}

func getAdvancedOptionInt(envName string, defaultValue int) int {
	valueStr := os.Getenv(envName)
	if valueStr == "" {
		return defaultValue
	}
	parsedValue, err := strconv.ParseInt(valueStr, 10, 32)
	if err != nil {
		logrus.Error(fmt.Sprintf("Failed to parse value of %s as integer. Using default %d. %v", envName, defaultValue, err))
		return defaultValue
	}
	return int(parsedValue)
}

type driver struct {
	mu     sync.Mutex
	logs   map[string]*logPair
	idx    map[string]*logPair
	logger logger.Logger
}

type logPair struct {
	jsonl   logger.Logger
	splunkl logger.Logger
	stream  io.ReadCloser
	info    logger.Info
}

func newDriver() *driver {
	return &driver{
		logs: make(map[string]*logPair),
		idx:  make(map[string]*logPair),
	}
}

func (d *driver) StartLogging(file string, logCtx logger.Info) error {
	d.mu.Lock()
	if _, exists := d.logs[file]; exists {
		d.mu.Unlock()
		return fmt.Errorf("logger for %q already exists", file)
	}
	d.mu.Unlock()

	if logCtx.LogPath == "" {
		logCtx.LogPath = filepath.Join("/var/log/docker", logCtx.ContainerID)
	}
	if err := os.MkdirAll(filepath.Dir(logCtx.LogPath), 0755); err != nil {
		return errors.Wrap(err, "error setting up logger dir")
	}
	jsonl, err := jsonfilelog.New(logCtx)
	if err != nil {
		return errors.Wrap(err, "error creating jsonfile logger")
	}

	err = ValidateLogOpt(logCtx.Config)
	if err != nil {
		return errors.Wrapf(err, "error options logger splunk: %q", file)
	}

	splunkl, err := New(logCtx)
	if err != nil {
		return errors.Wrap(err, "error creating splunk logger")
	}

	logrus.WithField("id", logCtx.ContainerID).WithField("file", file).WithField("logpath", logCtx.LogPath).Debugf("Start logging")
	f, err := fifo.OpenFifo(context.Background(), file, syscall.O_RDONLY, 0700)
	if err != nil {
		return errors.Wrapf(err, "error opening logger fifo: %q", file)
	}

	d.mu.Lock()
	lf := &logPair{jsonl, splunkl, f, logCtx}
	d.logs[file] = lf
	d.idx[logCtx.ContainerID] = lf
	d.mu.Unlock()

	go consumeLog(lf)
	return nil
}

func (d *driver) StopLogging(file string) error {
	logrus.WithField("file", file).Debugf("Stop logging")
	d.mu.Lock()
	lf, ok := d.logs[file]
	if ok {
		lf.stream.Close()
		delete(d.logs, file)
	}
	d.mu.Unlock()
	return nil
}

func sendMessage(l logger.Logger, buf *logdriver.LogEntry, containerid string) bool {
	var msg logger.Message
	msg.Line = buf.Line
	msg.Source = buf.Source
	msg.Partial = buf.Partial
	msg.Timestamp = time.Unix(0, buf.TimeNano)
	err := l.Log(&msg)
	if err != nil {
		logrus.WithField("id", containerid).WithError(err).WithField("message", msg).Error("error writing log message")
		return false
	}
	return true
}

func consumeLog(lf *logPair) {
	dec := protoio.NewUint32DelimitedReader(lf.stream, binary.BigEndian, 1e6)
	defer dec.Close()
	var buf logdriver.LogEntry
	for {
		if err := dec.ReadMsg(&buf); err != nil {
			if err == io.EOF {
				logrus.WithField("id", lf.info.ContainerID).WithError(err).Debug("shutting down log logger")
				lf.stream.Close()
				return
			}
			dec = protoio.NewUint32DelimitedReader(lf.stream, binary.BigEndian, 1e6)
		}

		if sendMessage(lf.splunkl, &buf, lf.info.ContainerID) == false {
			continue
		}
		if sendMessage(lf.jsonl, &buf, lf.info.ContainerID) == false {
			continue
		}

		buf.Reset()
	}
}

func (d *driver) ReadLogs(info logger.Info, config logger.ReadConfig) (io.ReadCloser, error) {
	d.mu.Lock()
	lf, exists := d.idx[info.ContainerID]
	d.mu.Unlock()
	if !exists {
		return nil, fmt.Errorf("logger does not exist for %s", info.ContainerID)
	}

	r, w := io.Pipe()
	lr, ok := lf.jsonl.(logger.LogReader)
	if !ok {
		return nil, fmt.Errorf("logger does not support reading")
	}

	go func() {
		watcher := lr.ReadLogs(config)

		enc := protoio.NewUint32DelimitedWriter(w, binary.BigEndian)
		defer enc.Close()
		defer watcher.Close()

		var buf logdriver.LogEntry
		for {
			select {
			case msg, ok := <-watcher.Msg:
				if !ok {
					w.Close()
					return
				}

				buf.Line = msg.Line
				buf.Partial = msg.Partial
				buf.TimeNano = msg.Timestamp.UnixNano()
				buf.Source = msg.Source

				if err := enc.WriteMsg(&buf); err != nil {
					w.CloseWithError(err)
					return
				}
			case err := <-watcher.Err:
				w.CloseWithError(err)
				return
			}

			buf.Reset()
		}
	}()

	return r, nil
}
