package main

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log"
	"net/http"
	"sync"
	"sync/atomic"
	"time"
)

type JSONRPCRequest struct {
	JSONRPC string                 `json:"jsonrpc"`
	ID      int                    `json:"id"`
	Method  string                 `json:"method"`
	Params  map[string]interface{} `json:"params"`
}

type BroadcastResult struct {
	Code      uint32 `json:"code"`
	Data      []byte `json:"data"`
	Log       string `json:"log"`
	Codespace string `json:"codespace"`
	Hash      string `json:"hash"`
}

type JSONRPCResponse struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      int             `json:"id"`
	Result  BroadcastResult `json:"result"`
	Error   interface{}     `json:"error"`
}

func main() {
	rpcAddr := flag.String("rpc", "http://127.0.0.1:26657", "RPC address")
	key := flag.String("key", "", "Key to set")
	value := flag.String("value", "", "Value to set")
	count := flag.Int("count", 1, "Number of transactions to send")
	rate := flag.Int("rate", 0, "Transactions per second (0 = unlimited)")
	workers := flag.Int("workers", 10, "Number of concurrent workers")
	flag.Parse()

	if *key == "" {
		log.Fatal("Please specify -key")
	}
	if *value == "" {
		log.Fatal("Please specify -value")
	}

	fmt.Printf("Connected to: %s\n", *rpcAddr)
	fmt.Printf("Sending %d transactions with %d workers...\n\n", *count, *workers)

	var successCount int64
	var failCount int64
	startTime := time.Now()

	// Calculate delay between transactions
	var delay time.Duration
	if *rate > 0 {
		delay = time.Second / time.Duration(*rate)
	}

	// Create work channel and wait group
	type txJob struct {
		index int
		data  string
	}
	jobs := make(chan txJob, *count)
	var wg sync.WaitGroup

	// Start workers
	for w := 0; w < *workers; w++ {
		wg.Add(1)
		go func(workerID int) {
			defer wg.Done()
			for job := range jobs {
				// Send transaction
				hash, err := broadcastTx(*rpcAddr, job.data, job.index+1)
				if err != nil {
					log.Printf("Worker %d: Failed to send tx %d: %v", workerID, job.index, err)
					atomic.AddInt64(&failCount, 1)
					continue
				}

				atomic.AddInt64(&successCount, 1)
				if *count <= 10 {
					// Only print details for small batches
					fmt.Printf("✅ Tx %d sent: %s (hash: %s)\n", job.index+1, job.data, hash)
				}
			}
		}(w)
	}

	// Send jobs to workers
	for i := 0; i < *count; i++ {
		// Create transaction: key=value
		var txString string
		if *count == 1 {
			txString = fmt.Sprintf("%s=%s", *key, *value)
		} else {
			// For batch sending, append counter to make unique
			txString = fmt.Sprintf("%s_%d=%s_%d", *key, i, *value, i)
		}

		jobs <- txJob{index: i, data: txString}

		// Rate limiting
		if delay > 0 {
			time.Sleep(delay)
		}
	}
	close(jobs)

	// Wait for all workers to finish
	wg.Wait()

	elapsed := time.Since(startTime)
	tps := float64(successCount) / elapsed.Seconds()

	fmt.Printf("\n━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━\n")
	fmt.Printf("Summary:\n")
	fmt.Printf("  Total sent:     %d\n", *count)
	fmt.Printf("  Successful:     %d\n", successCount)
	fmt.Printf("  Failed:         %d\n", failCount)
	fmt.Printf("  Time elapsed:   %v\n", elapsed)
	fmt.Printf("  TPS (sent):     %.2f tx/s\n", tps)
	fmt.Printf("  Workers:        %d\n", *workers)
	fmt.Printf("━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━\n")
}

func broadcastTx(rpcAddr, tx string, id int) (string, error) {
	// Encode transaction as base64
	txBase64 := base64.StdEncoding.EncodeToString([]byte(tx))

	// Create JSON-RPC request
	request := JSONRPCRequest{
		JSONRPC: "2.0",
		ID:      id,
		Method:  "broadcast_tx_sync",
		Params: map[string]interface{}{
			"tx": txBase64,
		},
	}

	requestBody, err := json.Marshal(request)
	if err != nil {
		return "", fmt.Errorf("failed to marshal request: %w", err)
	}

	// Send HTTP POST request
	resp, err := http.Post(rpcAddr, "application/json", bytes.NewBuffer(requestBody))
	if err != nil {
		return "", fmt.Errorf("failed to send request: %w", err)
	}
	defer resp.Body.Close()

	// Read response
	responseBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", fmt.Errorf("failed to read response: %w", err)
	}

	// Parse response
	var response JSONRPCResponse
	if err := json.Unmarshal(responseBody, &response); err != nil {
		return "", fmt.Errorf("failed to unmarshal response: %w", err)
	}

	if response.Error != nil {
		return "", fmt.Errorf("RPC error: %v", response.Error)
	}

	if response.Result.Code != 0 {
		return "", fmt.Errorf("transaction rejected: %s", response.Result.Log)
	}

	return response.Result.Hash, nil
}
