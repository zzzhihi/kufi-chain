package main

import (
	"bytes"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"math"
	"net/http"
	"os"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

type transferPayload struct {
	FromID         string `json:"from_id"`
	ToID           string `json:"to_id"`
	AmountVND      int64  `json:"amount_vnd"`
	Memo           string `json:"memo"`
	InternalRef    string `json:"internal_ref"`
	IdempotencyKey string `json:"idempotency_key"`
	Nonce          string `json:"nonce"`
	Timestamp      int64  `json:"timestamp"`
	TransferType   string `json:"transfer_type,omitempty"`
}

type endpointStat struct {
	Sent    int `json:"sent"`
	Success int `json:"success"`
	Valid   int `json:"valid"`
}

type requestResult struct {
	Endpoint   string
	HTTPStatus int
	Success    bool
	Valid      bool
	LatencyMS  float64
	ErrorKey   string
}

type report struct {
	StartedAt       time.Time               `json:"started_at"`
	FinishedAt      time.Time               `json:"finished_at"`
	DurationSec     float64                 `json:"duration_sec"`
	Total           int                     `json:"total"`
	Workers         int                     `json:"workers"`
	SentTPS         float64                 `json:"sent_tps"`
	SuccessCount    int                     `json:"success_count"`
	SuccessRate     float64                 `json:"success_rate"`
	SuccessTPS      float64                 `json:"success_tps"`
	ValidCount      int                     `json:"valid_count"`
	ValidRate       float64                 `json:"valid_rate"`
	ValidTPS        float64                 `json:"valid_tps"`
	HTTPStatusCodes map[int]int             `json:"http_status_codes"`
	TopErrors       map[string]int          `json:"top_errors"`
	EndpointStats   map[string]endpointStat `json:"endpoint_stats"`
	LatencyAllMS    latencyReport           `json:"latency_all_ms"`
	LatencyOKMS     latencyReport           `json:"latency_success_ms"`
}

type latencyReport struct {
	Count int     `json:"count"`
	Min   float64 `json:"min"`
	Avg   float64 `json:"avg"`
	P50   float64 `json:"p50"`
	P95   float64 `json:"p95"`
	P99   float64 `json:"p99"`
	Max   float64 `json:"max"`
}

type gatewayResponse struct {
	Success bool `json:"success"`
	TxID    string `json:"tx_id"`
	Receipt struct {
		ValidationCodeName string `json:"validation_code_name"`
	} `json:"receipt"`
	Error map[string]interface{} `json:"error"`
}

func main() {
	var (
		endpointsCSV = flag.String("endpoints", "http://127.0.0.1:8080", "Comma-separated gateway base URLs, e.g. http://18.141.70.237:8080,http://3.1.105.238:8080")
		total        = flag.Int("total", 10000, "Total number of transfer requests")
		workers      = flag.Int("workers", 64, "Number of concurrent workers")
		timeout      = flag.Duration("timeout", 20*time.Second, "HTTP timeout per request")
		amount       = flag.Int64("amount", 1000, "Amount VND per transfer")
		fromID       = flag.String("from", "alice", "from_id")
		toID         = flag.String("to", "bob", "to_id")
		memo         = flag.String("memo", "throughput-test", "memo")
		prefix       = flag.String("prefix", "bench", "internal_ref/idempotency prefix")
		transferType = flag.String("transfer-type", "intra_bank", "transfer_type payload field")
		outPath      = flag.String("out", "", "Optional path to write full JSON report")
	)
	flag.Parse()

	if *total <= 0 {
		fatalf("total must be > 0")
	}
	if *workers <= 0 {
		fatalf("workers must be > 0")
	}

	endpoints := parseEndpoints(*endpointsCSV)
	if len(endpoints) == 0 {
		fatalf("no valid endpoints provided")
	}

	client := &http.Client{
		Timeout: *timeout,
		Transport: &http.Transport{
			MaxIdleConns:        *workers * 4,
			MaxIdleConnsPerHost: *workers * 2,
			IdleConnTimeout:     90 * time.Second,
		},
	}

	fmt.Printf("Benchmark start: total=%d workers=%d endpoints=%d timeout=%s\n", *total, *workers, len(endpoints), timeout.String())
	fmt.Printf("Endpoints: %s\n", strings.Join(endpoints, ", "))

	jobs := make(chan int)
	results := make(chan requestResult, *workers*2)

	var endpointCounter uint64
	var wg sync.WaitGroup

	startedAt := time.Now()
	runID := startedAt.UnixNano()

	for w := 0; w < *workers; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := range jobs {
				endpoint := endpoints[int(atomic.AddUint64(&endpointCounter, 1)-1)%len(endpoints)]
				results <- sendOne(client, endpoint, buildPayload(*prefix, runID, i, *fromID, *toID, *amount, *memo, *transferType))
			}
		}()
	}

	go func() {
		for i := 0; i < *total; i++ {
			jobs <- i
		}
		close(jobs)
		wg.Wait()
		close(results)
	}()

	rep := aggregate(startedAt, *total, *workers, results)
	printSummary(rep)

	if *outPath != "" {
		if err := writeReport(*outPath, rep); err != nil {
			fatalf("write report: %v", err)
		}
		fmt.Printf("Report written: %s\n", *outPath)
	}
}

func parseEndpoints(raw string) []string {
	parts := strings.Split(raw, ",")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		p = strings.TrimSpace(p)
		if p == "" {
			continue
		}
		p = strings.TrimRight(p, "/")
		out = append(out, p)
	}
	return out
}

func buildPayload(prefix string, runID int64, idx int, fromID, toID string, amount int64, memo, transferType string) transferPayload {
	nowMS := time.Now().UnixMilli()
	internalRef := fmt.Sprintf("%s-ref-%d-%d", prefix, runID, idx)
	idem := fmt.Sprintf("%s-idem-%d-%d", prefix, runID, idx)
	nonce := fmt.Sprintf("%s-nonce-%d-%d", prefix, runID, idx)

	return transferPayload{
		FromID:         fromID,
		ToID:           toID,
		AmountVND:      amount,
		Memo:           memo,
		InternalRef:    internalRef,
		IdempotencyKey: idem,
		Nonce:          nonce,
		Timestamp:      nowMS,
		TransferType:   transferType,
	}
}

func sendOne(client *http.Client, endpoint string, payload transferPayload) requestResult {
	start := time.Now()

	body, err := json.Marshal(payload)
	if err != nil {
		return requestResult{
			Endpoint:  endpoint,
			LatencyMS: elapsedMS(start),
			ErrorKey:  "marshal_payload",
		}
	}

	req, err := http.NewRequestWithContext(context.Background(), http.MethodPost, endpoint+"/v1/transfer", bytes.NewReader(body))
	if err != nil {
		return requestResult{
			Endpoint:  endpoint,
			LatencyMS: elapsedMS(start),
			ErrorKey:  "build_request",
		}
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := client.Do(req)
	if err != nil {
		return requestResult{
			Endpoint:  endpoint,
			LatencyMS: elapsedMS(start),
			ErrorKey:  classifyNetworkErr(err),
		}
	}
	defer resp.Body.Close()

	data, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	latMS := elapsedMS(start)

	var gw gatewayResponse
	if err := json.Unmarshal(data, &gw); err != nil {
		return requestResult{
			Endpoint:   endpoint,
			HTTPStatus: resp.StatusCode,
			LatencyMS:  latMS,
			ErrorKey:   "invalid_json_response",
		}
	}

	success := resp.StatusCode >= 200 && resp.StatusCode < 300 && gw.Success && gw.TxID != ""
	valid := strings.EqualFold(strings.TrimSpace(gw.Receipt.ValidationCodeName), "VALID")

	errKey := ""
	if !success {
		errKey = pickErrorKey(resp.StatusCode, gw.Error)
	}

	return requestResult{
		Endpoint:   endpoint,
		HTTPStatus: resp.StatusCode,
		Success:    success,
		Valid:      success && valid,
		LatencyMS:  latMS,
		ErrorKey:   errKey,
	}
}

func aggregate(startedAt time.Time, total, workers int, results <-chan requestResult) report {
	var (
		finishedAt = time.Now()
		allLat     = make([]float64, 0, total)
		okLat      = make([]float64, 0, total)
		success    int
		valid      int
		statuses   = map[int]int{}
		errors     = map[string]int{}
		epStats    = map[string]endpointStat{}
	)

	for r := range results {
		finishedAt = time.Now()
		allLat = append(allLat, r.LatencyMS)

		if r.HTTPStatus != 0 {
			statuses[r.HTTPStatus]++
		}

		st := epStats[r.Endpoint]
		st.Sent++
		if r.Success {
			success++
			st.Success++
			okLat = append(okLat, r.LatencyMS)
		}
		if r.Valid {
			valid++
			st.Valid++
		}
		epStats[r.Endpoint] = st

		if r.ErrorKey != "" {
			errors[r.ErrorKey]++
		}
	}

	durSec := finishedAt.Sub(startedAt).Seconds()
	if durSec <= 0 {
		durSec = 0.001
	}

	successRate := float64(success) * 100 / float64(total)
	validRate := float64(valid) * 100 / float64(total)

	return report{
		StartedAt:       startedAt,
		FinishedAt:      finishedAt,
		DurationSec:     durSec,
		Total:           total,
		Workers:         workers,
		SentTPS:         float64(total) / durSec,
		SuccessCount:    success,
		SuccessRate:     successRate,
		SuccessTPS:      float64(success) / durSec,
		ValidCount:      valid,
		ValidRate:       validRate,
		ValidTPS:        float64(valid) / durSec,
		HTTPStatusCodes: statuses,
		TopErrors:       errors,
		EndpointStats:   epStats,
		LatencyAllMS:    computeLatencyReport(allLat),
		LatencyOKMS:     computeLatencyReport(okLat),
	}
}

func computeLatencyReport(vals []float64) latencyReport {
	if len(vals) == 0 {
		return latencyReport{}
	}
	sort.Float64s(vals)
	var sum float64
	for _, v := range vals {
		sum += v
	}
	return latencyReport{
		Count: len(vals),
		Min:   round2(vals[0]),
		Avg:   round2(sum / float64(len(vals))),
		P50:   round2(percentile(vals, 0.50)),
		P95:   round2(percentile(vals, 0.95)),
		P99:   round2(percentile(vals, 0.99)),
		Max:   round2(vals[len(vals)-1]),
	}
}

func percentile(sortedVals []float64, p float64) float64 {
	if len(sortedVals) == 0 {
		return 0
	}
	if p <= 0 {
		return sortedVals[0]
	}
	if p >= 1 {
		return sortedVals[len(sortedVals)-1]
	}
	rank := int(math.Ceil(p*float64(len(sortedVals)))) - 1
	if rank < 0 {
		rank = 0
	}
	if rank >= len(sortedVals) {
		rank = len(sortedVals) - 1
	}
	return sortedVals[rank]
}

func round2(v float64) float64 {
	return math.Round(v*100) / 100
}

func elapsedMS(start time.Time) float64 {
	return float64(time.Since(start).Microseconds()) / 1000
}

func classifyNetworkErr(err error) string {
	msg := strings.ToLower(err.Error())
	switch {
	case strings.Contains(msg, "timeout"):
		return "network_timeout"
	case strings.Contains(msg, "connection refused"):
		return "connection_refused"
	case strings.Contains(msg, "no such host"):
		return "dns_error"
	default:
		return "network_error"
	}
}

func pickErrorKey(httpStatus int, payload map[string]interface{}) string {
	if msg, ok := payload["message"].(string); ok && strings.TrimSpace(msg) != "" {
		return fmt.Sprintf("http_%d:%s", httpStatus, trimForKey(msg))
	}
	if msg, ok := payload["details"].(string); ok && strings.TrimSpace(msg) != "" {
		return fmt.Sprintf("http_%d:%s", httpStatus, trimForKey(msg))
	}
	if msg, ok := payload["error"].(string); ok && strings.TrimSpace(msg) != "" {
		return fmt.Sprintf("http_%d:%s", httpStatus, trimForKey(msg))
	}
	return fmt.Sprintf("http_%d", httpStatus)
}

func trimForKey(s string) string {
	s = strings.TrimSpace(strings.ToLower(s))
	if len(s) > 80 {
		return s[:80]
	}
	return s
}

func writeReport(path string, rep report) error {
	data, err := json.MarshalIndent(rep, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, data, 0o644)
}

func printSummary(rep report) {
	fmt.Println("")
	fmt.Println("===== Benchmark Summary =====")
	fmt.Printf("Duration:        %.2fs\n", rep.DurationSec)
	fmt.Printf("Total sent:      %d\n", rep.Total)
	fmt.Printf("Workers:         %d\n", rep.Workers)
	fmt.Printf("Throughput sent: %.2f req/s\n", rep.SentTPS)
	fmt.Printf("Success:         %d (%.2f%%)\n", rep.SuccessCount, rep.SuccessRate)
	fmt.Printf("Throughput ok:   %.2f tx/s\n", rep.SuccessTPS)
	fmt.Printf("VALID receipts:  %d (%.2f%%)\n", rep.ValidCount, rep.ValidRate)
	fmt.Printf("Throughput VALID %.2f tx/s\n", rep.ValidTPS)
	fmt.Println("")
	fmt.Println("Latency (all requests) [ms]:")
	fmt.Printf("  min/avg/p50/p95/p99/max = %.2f / %.2f / %.2f / %.2f / %.2f / %.2f\n",
		rep.LatencyAllMS.Min, rep.LatencyAllMS.Avg, rep.LatencyAllMS.P50,
		rep.LatencyAllMS.P95, rep.LatencyAllMS.P99, rep.LatencyAllMS.Max)
	fmt.Println("Latency (successful requests) [ms]:")
	fmt.Printf("  min/avg/p50/p95/p99/max = %.2f / %.2f / %.2f / %.2f / %.2f / %.2f\n",
		rep.LatencyOKMS.Min, rep.LatencyOKMS.Avg, rep.LatencyOKMS.P50,
		rep.LatencyOKMS.P95, rep.LatencyOKMS.P99, rep.LatencyOKMS.Max)

	fmt.Println("")
	fmt.Println("HTTP status distribution:")
	if len(rep.HTTPStatusCodes) == 0 {
		fmt.Println("  (none)")
	} else {
		keys := make([]int, 0, len(rep.HTTPStatusCodes))
		for code := range rep.HTTPStatusCodes {
			keys = append(keys, code)
		}
		sort.Ints(keys)
		for _, code := range keys {
			fmt.Printf("  %d => %d\n", code, rep.HTTPStatusCodes[code])
		}
	}

	if len(rep.TopErrors) > 0 {
		fmt.Println("")
		fmt.Println("Top errors:")
		type pair struct {
			k string
			v int
		}
		list := make([]pair, 0, len(rep.TopErrors))
		for k, v := range rep.TopErrors {
			list = append(list, pair{k: k, v: v})
		}
		sort.Slice(list, func(i, j int) bool {
			if list[i].v == list[j].v {
				return list[i].k < list[j].k
			}
			return list[i].v > list[j].v
		})
		limit := 10
		if len(list) < limit {
			limit = len(list)
		}
		for i := 0; i < limit; i++ {
			fmt.Printf("  %s => %d\n", list[i].k, list[i].v)
		}
	}

	fmt.Println("")
	fmt.Println("Per-endpoint:")
	endpoints := make([]string, 0, len(rep.EndpointStats))
	for ep := range rep.EndpointStats {
		endpoints = append(endpoints, ep)
	}
	sort.Strings(endpoints)
	for _, ep := range endpoints {
		st := rep.EndpointStats[ep]
		fmt.Printf("  %s => sent=%d success=%d valid=%d\n", ep, st.Sent, st.Success, st.Valid)
	}
}

func fatalf(format string, a ...interface{}) {
	fmt.Fprintf(os.Stderr, "ERROR: "+format+"\n", a...)
	os.Exit(1)
}
