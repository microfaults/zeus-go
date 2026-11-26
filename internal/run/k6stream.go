package run

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"io"

	"atropos-go/loadgen/internal/stats"
)

// maxSampleLine bounds the memory of one k6 JSON output line. Real sample
// lines are a few hundred bytes; a longer one (e.g. an enormous url tag) is
// skipped, not buffered.
const maxSampleLine = 64 << 10

// k6Envelope is the subset of a k6 JSON output line (`--out json=...`) that
// LiveRun needs. Lines are either {"type":"Metric",...} declarations or
// {"metric":"<name>","type":"Point","data":{"time":...,"value":...,"tags":{...}}}
// samples.
type k6Envelope struct {
	Type   string `json:"type"`
	Metric string `json:"metric"`
	Data   struct {
		Value float64           `json:"value"`
		Tags  map[string]string `json:"tags"`
	} `json:"data"`
}

// streamSamples reads k6 JSON output from r until EOF and feeds every sample
// LiveRun understands into live. Each line is decoded and dropped; nothing
// is retained. It always reads to EOF -- k6 blocks on a full pipe, so
// stopping early would stall the run -- and returns the number of lines it
// could not decode (skipped).
func streamSamples(r io.Reader, live *stats.LiveRun) (bad int, err error) {
	br := bufio.NewReaderSize(r, maxSampleLine)
	for {
		line, err := br.ReadSlice('\n')
		if errors.Is(err, bufio.ErrBufferFull) {
			// Oversized line: discard through its newline.
			for errors.Is(err, bufio.ErrBufferFull) {
				_, err = br.ReadSlice('\n')
			}
			bad++
			if err == nil {
				continue
			}
			line = nil
		}
		if len(line) > 0 && !handleSampleLine(line, live) {
			bad++
		}
		if err != nil {
			if errors.Is(err, io.EOF) {
				return bad, nil
			}
			return bad, err
		}
	}
}

var pointPrefix = []byte(`{"metric":"`)

// handleSampleLine ingests one line; false = undecodable.
func handleSampleLine(line []byte, live *stats.LiveRun) bool {
	line = bytes.TrimSpace(line)
	if len(line) == 0 {
		return true
	}
	// Fast path: k6 writes Points as {"metric":"<name>",...}; skip metrics
	// LiveRun ignores (http_req_blocked, data_sent, checks, ...) undecoded.
	if bytes.HasPrefix(line, pointPrefix) {
		rest := line[len(pointPrefix):]
		if end := bytes.IndexByte(rest, '"'); end >= 0 && !stats.IsLiveMetric(string(rest[:end])) {
			return true
		}
	}
	var env k6Envelope
	if err := json.Unmarshal(line, &env); err != nil {
		return false
	}
	if env.Type != "Point" || !stats.IsLiveMetric(env.Metric) {
		return true
	}
	live.Observe(env.Metric, env.Data.Value, env.Data.Tags["name"])
	return true
}
