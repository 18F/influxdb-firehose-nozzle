package influxdbclient

import (
	"bytes"
	"fmt"
	"net/http"
	"strconv"
	"time"

	"errors"
	"log"

	"github.com/cloudfoundry/sonde-go/events"
)

type Client struct {
	url                   string
	database              string
	user                  string
	password              string
	metricPoints          map[metricKey]metricValue
	prefix                string
	deployment            string
	ip                    string
	totalMessagesReceived uint64
	totalMetricsSent      uint64
}

type metricKey struct {
	eventType  events.Envelope_EventType
	name       string
	deployment string
	job        string
	index      string
	ip         string
}

type metricValue struct {
	tags   []string
	points []Point
}

type Metric struct {
	Metric string   `json:"metric"`
	Points []Point  `json:"points"`
	Type   string   `json:"type"`
	Host   string   `json:"host,omitempty"`
	Tags   []string `json:"tags,omitempty"`
}

type Point struct {
	Timestamp int64
	Value     float64
}

func New(url string, database string, user string, password string, prefix string, deployment string, ip string) *Client {
	return &Client{
		url:          url,
		database:     database,
		user:         user,
		password:     password,
		metricPoints: make(map[metricKey]metricValue),
		prefix:       prefix,
		deployment:   deployment,
		ip:           ip,
	}
}

func (c *Client) AlertSlowConsumerError() {
	c.addInternalMetric("slowConsumerAlert", uint64(1))
}

func (c *Client) AddMetric(envelope *events.Envelope) {
	c.totalMessagesReceived++
	if envelope.GetEventType() != events.Envelope_ValueMetric && envelope.GetEventType() != events.Envelope_CounterEvent {
		return
	}

	key := metricKey{
		eventType:  envelope.GetEventType(),
		name:       getName(envelope),
		deployment: envelope.GetDeployment(),
		job:        envelope.GetJob(),
		index:      envelope.GetIndex(),
		ip:         envelope.GetIp(),
	}

	mVal := c.metricPoints[key]
	value := getValue(envelope)

	mVal.tags = getTags(envelope)
	mVal.points = append(mVal.points, Point{
		Timestamp: envelope.GetTimestamp() / int64(time.Second),
		Value:     value,
	})

	c.metricPoints[key] = mVal
}

func (c *Client) PostMetrics() error {
	url := c.seriesURL()

	c.populateInternalMetrics()
	numMetrics := len(c.metricPoints)
	log.Printf("Posting %d metrics", numMetrics)

	seriesBytes, metricsCount := c.formatMetrics()

	req, err := http.NewRequest("POST", url, bytes.NewBuffer(seriesBytes))
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}

	defer resp.Body.Close()
	if resp.StatusCode >= 300 || resp.StatusCode < 200 {
		return fmt.Errorf("InfluxDB request returned HTTP response: %s", resp.Status)
	}

	c.totalMetricsSent += metricsCount
	c.metricPoints = make(map[metricKey]metricValue)

	return nil
}

func (c *Client) seriesURL() string {
	url := fmt.Sprintf("%s/write?db=%s", c.url, c.database)
	log.Print("Using the following influx URL " + url)
	return url
}

func (c *Client) populateInternalMetrics() {
	c.addInternalMetric("totalMessagesReceived", c.totalMessagesReceived)
	c.addInternalMetric("totalMetricsSent", c.totalMetricsSent)

	if !c.containsSlowConsumerAlert() {
		c.addInternalMetric("slowConsumerAlert", uint64(0))
	}
}

func (c *Client) containsSlowConsumerAlert() bool {
	key := metricKey{
		name:       "slowConsumerAlert",
		deployment: c.deployment,
		ip:         c.ip,
	}
	_, ok := c.metricPoints[key]
	return ok
}

func (c *Client) formatMetrics() ([]byte, uint64) {
	var buffer bytes.Buffer

	for key, mVal := range c.metricPoints {
		buffer.WriteString(c.prefix + key.name)
		buffer.WriteString(",")
		buffer.WriteString(formatTags(mVal.tags))
		buffer.WriteString(" ")
		buffer.WriteString(formatValues(mVal.points))
		buffer.WriteString(" ")
		buffer.WriteString(formatTimestamp(mVal.points))
		buffer.WriteString("\n")
	}

	return buffer.Bytes(), uint64(len(c.metricPoints))
}

func formatTags(tags []string) string {
	var newTags string
	for index, tag := range tags {
		if index > 0 {
			newTags += ","
		}

		newTags += tag
	}
	return newTags
}

func formatValues(points []Point) string {
	var newPoints string
	for index, point := range points {
		if index > 0 {
			newPoints += ","
		}

		newPoints += "value=" + strconv.FormatFloat(point.Value, 'f', -1, 64)
	}
	return newPoints
}

func formatTimestamp(points []Point) string {
	if len(points) > 0 {
		return strconv.FormatInt(points[0].Timestamp*1000*1000*1000, 10)
	} else {
		return strconv.FormatInt(time.Now().Unix()*1000*1000*1000, 10)
	}
}

func (c *Client) addInternalMetric(name string, value uint64) {
	key := metricKey{
		name:       name,
		deployment: c.deployment,
		ip:         c.ip,
	}

	point := Point{
		Timestamp: time.Now().Unix(),
		Value:     float64(value),
	}

	mValue := metricValue{
		tags: []string{
			fmt.Sprintf("ip=%s", c.ip),
			fmt.Sprintf("deployment=%s", c.deployment),
		},
		points: []Point{point},
	}

	c.metricPoints[key] = mValue
}

func getName(envelope *events.Envelope) string {
	switch envelope.GetEventType() {
	case events.Envelope_ValueMetric:
		return envelope.GetOrigin() + "." + envelope.GetValueMetric().GetName()
	case events.Envelope_CounterEvent:
		return envelope.GetOrigin() + "." + envelope.GetCounterEvent().GetName()
	default:
		panic("Unknown event type")
	}
}

func getValue(envelope *events.Envelope) float64 {
	switch envelope.GetEventType() {
	case events.Envelope_ValueMetric:
		return envelope.GetValueMetric().GetValue()
	case events.Envelope_CounterEvent:
		return float64(envelope.GetCounterEvent().GetTotal())
	default:
		panic("Unknown event type")
	}
}

func getTags(envelope *events.Envelope) []string {
	var tags []string

	tags = appendTagIfNotEmpty(tags, "deployment", envelope.GetDeployment())
	tags = appendTagIfNotEmpty(tags, "job", envelope.GetJob())
	tags = appendTagIfNotEmpty(tags, "index", envelope.GetIndex())
	tags = appendTagIfNotEmpty(tags, "ip", envelope.GetIp())

	return tags
}

func appendTagIfNotEmpty(tags []string, key string, value string) []string {
	if value != "" {
		tags = append(tags, fmt.Sprintf("%s=%s", key, value))
	}
	return tags
}

func (p Point) MarshalJSON() ([]byte, error) {
	return []byte(fmt.Sprintf(`[%d, %f]`, p.Timestamp, p.Value)), nil
}

func (p *Point) UnmarshalJSON(in []byte) error {
	var timestamp int64
	var value float64

	parsed, err := fmt.Sscanf(string(in), `[%d,%f]`, &timestamp, &value)
	if err != nil {
		return err
	}
	if parsed != 2 {
		return errors.New("expected two parsed values")
	}

	p.Timestamp = timestamp
	p.Value = value

	return nil
}
